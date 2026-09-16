package chatcompletion

import (
	"encoding/json"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// @AI_GUARD: CC_USAGE_DETAILS - 上游嵌套 *_tokens_details 是唯一权威来源
// @REASON: v0.2.148 及之前本包 Usage 只有三键，OpenAI 官方格式把 cached / cache_write /
//   reasoning 放在 usage.prompt_tokens_details / completion_tokens_details 里。反序列化时
//   未知键被静默丢弃，所以「usage 看着有值、cache 恒为 0」且编译期零提示。
//   感诺把 stream_options.include_usage 的统计量放在 choices:[] 的独立尾帧，
//   该尾帧与完整响应体共用本结构体，两处都要能读。

// TestToInternalUsage_NestedDetailsPreferred 嵌套字段是权威值，顶层旧格式键不能覆盖它。
// 真实上游（OpenAI 官方）两种都发，嵌套是规范位置。
func TestToInternalUsage_NestedDetailsPreferred(t *testing.T) {
	u := &Usage{
		PromptTokens: 12000, CompletionTokens: 400, TotalTokens: 12400,
		// 故意与嵌套不一致：嵌套才是权威
		CacheCreationTokens:     111,
		CacheReadTokens:         222,
		PromptTokensDetails:     &PromptTokensDetails{CachedTokens: 10000, CacheWriteTokens: 2000},
		CompletionTokensDetails: &CompletionTokensDetails{ReasoningTokens: 150},
	}
	iu := ToInternalUsage(u)
	if iu.PromptTokens != 12000 || iu.CompletionTokens != 400 || iu.TotalTokens != 12400 {
		t.Errorf("三键 = %d/%d/%d, want 12000/400/12400",
			iu.PromptTokens, iu.CompletionTokens, iu.TotalTokens)
	}
	if iu.CacheReadTokens != 10000 {
		t.Errorf("CacheReadTokens = %d, want 10000（嵌套优先于顶层 222）", iu.CacheReadTokens)
	}
	if iu.CacheWriteTokens != 2000 {
		t.Errorf("CacheWriteTokens = %d, want 2000（嵌套优先于顶层 111）", iu.CacheWriteTokens)
	}
	if iu.ReasoningTokens != 150 {
		t.Errorf("ReasoningTokens = %d, want 150", iu.ReasoningTokens)
	}
}

// TestToInternalUsage_LegacyTopLevelOnly 只发顶层旧格式键的上游不能丢。
func TestToInternalUsage_LegacyTopLevelOnly(t *testing.T) {
	u := &Usage{
		PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100,
		CacheCreationTokens: 250,
		CacheReadTokens:     700,
	}
	iu := ToInternalUsage(u)
	if iu.CacheReadTokens != 700 {
		t.Errorf("CacheReadTokens = %d, want 700", iu.CacheReadTokens)
	}
	if iu.CacheWriteTokens != 250 {
		t.Errorf("CacheWriteTokens = %d, want 250", iu.CacheWriteTokens)
	}
	if iu.ReasoningTokens != 0 {
		t.Errorf("ReasoningTokens = %d, want 0", iu.ReasoningTokens)
	}
}

// TestToInternalUsage_Nil 容忍 nil，与 server/gateway.go mapInternalUsage 的调用方式一致。
func TestToInternalUsage_Nil(t *testing.T) {
	if got := ToInternalUsage(nil); got != nil {
		t.Errorf("ToInternalUsage(nil) = %v, want nil", got)
	}
}

// TestUsage_JSONRoundTrip 锁住嵌套键名逐字节匹配上游格式。
// 只改字段名不改结构时本测试会失败——这正是唯一的回归方式。
func TestUsage_JSONRoundTrip(t *testing.T) {
	in := []byte(`{"prompt_tokens":12000,"completion_tokens":400,"total_tokens":12400,` +
		`"prompt_tokens_details":{"cached_tokens":10000,"cache_write_tokens":2000},` +
		`"completion_tokens_details":{"reasoning_tokens":150}}`)
	var u Usage
	if err := json.Unmarshal(in, &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if u.PromptTokensDetails == nil || u.PromptTokensDetails.CachedTokens != 10000 {
		t.Fatalf("prompt_tokens_details.cached_tokens 未解析到 %+v", u.PromptTokensDetails)
	}
	if u.PromptTokensDetails.CacheWriteTokens != 2000 {
		t.Errorf("cache_write_tokens = %d, want 2000", u.PromptTokensDetails.CacheWriteTokens)
	}
	if u.CompletionTokensDetails == nil || u.CompletionTokensDetails.ReasoningTokens != 150 {
		t.Fatalf("completion_tokens_details.reasoning_tokens 未解析到 %+v", u.CompletionTokensDetails)
	}

	// 出站必须写回同样的键名（Codex 走 CC 上游时也是这个形状）
	raw, err := json.Marshal(&u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"prompt_tokens_details", "completion_tokens_details",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("出站 usage 缺少 %q", key)
		}
	}
}

// TestUsage_OmitsEmptyDetails 全零时不得发出空 details 对象（Option 语义）。
func TestUsage_OmitsEmptyDetails(t *testing.T) {
	raw, err := json.Marshal(&Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"prompt_tokens_details", "completion_tokens_details"} {
		if _, ok := got[key]; ok {
			t.Errorf("全零时仍序列化出 %q（空对象等于「明确无缓存」，应为缺字段）", key)
		}
	}
}

// TestInternalToCCResponse_CarriesDetails 端到端：InternalUsage 带 cache/reasoning 时，
// 出站 CC 响应体必须带嵌套 details。
func TestInternalToCCResponse_CarriesDetails(t *testing.T) {
	resp := makeInternalRespForUsage(10000, 500, 10500, 8000, 2000, 300)
	out := InternalToCCResponse(resp)
	if out.Usage == nil {
		t.Fatal("Usage 为 nil（CC 出站必须是非空对象）")
	}
	if out.Usage.PromptTokensDetails == nil {
		t.Fatalf("prompt_tokens_details 为 nil\n%+v", out.Usage)
	}
	if out.Usage.PromptTokensDetails.CachedTokens != 8000 {
		t.Errorf("cached_tokens = %d, want 8000", out.Usage.PromptTokensDetails.CachedTokens)
	}
	if out.Usage.PromptTokensDetails.CacheWriteTokens != 2000 {
		t.Errorf("cache_write_tokens = %d, want 2000", out.Usage.PromptTokensDetails.CacheWriteTokens)
	}
	if out.Usage.CompletionTokensDetails == nil || out.Usage.CompletionTokensDetails.ReasoningTokens != 300 {
		t.Errorf("reasoning_tokens 丢失：%+v", out.Usage.CompletionTokensDetails)
	}
}

// makeInternalRespForUsage 构造只带 usage 的最小 InternalResponse，供出站测试使用。
func makeInternalRespForUsage(prompt, completion, total, cacheRead, cacheWrite, reasoning int) *schema.InternalResponse {
	return &schema.InternalResponse{
		ID:    "resp_test",
		Model: "glm-5.2",
		Usage: &schema.InternalUsage{
			PromptTokens:     prompt,
			CompletionTokens: completion,
			TotalTokens:      total,
			CacheReadTokens:  cacheRead,
			CacheWriteTokens: cacheWrite,
			ReasoningTokens:  reasoning,
		},
	}
}

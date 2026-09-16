package responses

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// runStream 已定义在 usage_frame_test.go（同包），此处复用。

// @AI_GUARD: RESPONSES_USAGE_DETAILS_NESTED - 出站 usage 的嵌套 details 唯一出口
// @REASON: Codex 的 serde 模型只读 usage.input_tokens_details.cached_tokens /
//   cache_write_tokens 与 usage.output_tokens_details.reasoning_tokens。v0.2.148 及之前代理
//   只写顶层 cache_creation_input_tokens / cache_read_input_tokens，顶层键被 serde 静默忽略，
//   usage 三个必需键齐全所以解析不失败——纯静默丢失，代理日志完全看不出来。
// @RELATED: types.go Usage/InputTokensDetails/OutputTokensDetails、
//   server/quick.go writeNonStreamAsSSE（同形状约束）

// TestResponsesUsageFromInternal_NestedDetails 锁住出站映射：
// 四个中枢字段必须同时出现在顶层与嵌套位置，且嵌套键名必须与 Codex 读的完全一致。
func TestResponsesUsageFromInternal_NestedDetails(t *testing.T) {
	in := &schema.InternalUsage{
		PromptTokens:     10000,
		CompletionTokens: 500,
		TotalTokens:      10500,
		CacheReadTokens:  8000,
		CacheWriteTokens: 2000,
		ReasoningTokens:  300,
	}
	out := responsesUsageFromInternal(in)
	if out == nil {
		t.Fatal("responsesUsageFromInternal 返回 nil，want 非 nil")
	}

	// 顶层三键：Codex 的必需键，缺一个就整帧解析失败
	if out.InputTokens != 10000 || out.OutputTokens != 500 || out.TotalTokens != 10500 {
		t.Errorf("顶层三键 = %d/%d/%d, want 10000/500/10500",
			out.InputTokens, out.OutputTokens, out.TotalTokens)
	}

	// 嵌套 details：Codex 真正读的字段
	if out.InputTokensDetails == nil {
		t.Fatal("InputTokensDetails 为 nil，Codex 将读到 cached=0 / cache_write=0")
	}
	if out.InputTokensDetails.CachedTokens != 8000 {
		t.Errorf("input_tokens_details.cached_tokens = %d, want 8000", out.InputTokensDetails.CachedTokens)
	}
	if out.InputTokensDetails.CacheWriteTokens != 2000 {
		t.Errorf("input_tokens_details.cache_write_tokens = %d, want 2000", out.InputTokensDetails.CacheWriteTokens)
	}
	if out.OutputTokensDetails == nil {
		t.Fatal("OutputTokensDetails 为 nil，Codex 将读到 reasoning=0")
	}
	if out.OutputTokensDetails.ReasoningTokens != 300 {
		t.Errorf("output_tokens_details.reasoning_tokens = %d, want 300", out.OutputTokensDetails.ReasoningTokens)
	}

	// 序列化后的键名必须逐字节匹配 Codex 的 serde 模型
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"input_tokens", "output_tokens", "total_tokens",
		"input_tokens_details", "output_tokens_details",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("出站 usage 缺少键 %q（Codex 必需或只读此位置）", key)
		}
	}
	var inDet struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	}
	if err := json.Unmarshal(got["input_tokens_details"], &inDet); err != nil {
		t.Fatalf("input_tokens_details 不是对象：%v", err)
	}
	if inDet.CachedTokens != 8000 || inDet.CacheWriteTokens != 2000 {
		t.Errorf("序列化后的 details = %+v, want {8000 2000}", inDet)
	}
}

// TestResponsesUsageFromInternal_OmitsEmptyDetails details 是 Option 语义：
// 全零时禁止发 {"cached_tokens":0,...}——空对象等于「上游明确声明无缓存」，
// 比缺字段更容易被下游误读成「不支持缓存」。
func TestResponsesUsageFromInternal_OmitsEmptyDetails(t *testing.T) {
	out := responsesUsageFromInternal(&schema.InternalUsage{
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
	})
	if out.InputTokensDetails != nil {
		t.Errorf("无缓存时 InputTokensDetails = %+v, want nil（不应发空对象）", out.InputTokensDetails)
	}
	if out.OutputTokensDetails != nil {
		t.Errorf("无 reasoning 时 OutputTokensDetails = %+v, want nil（不应发空对象）", out.OutputTokensDetails)
	}
	raw, _ := json.Marshal(out)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["input_tokens_details"]; ok {
		t.Error("无缓存时序列化出了 input_tokens_details，want 缺字段")
	}
	if _, ok := got["output_tokens_details"]; ok {
		t.Error("无 reasoning 时序列化出了 output_tokens_details，want 缺字段")
	}
	// 必需三键仍必须齐全：Codex 缺任一则整帧解析失败
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if _, ok := got[key]; !ok {
			t.Errorf("缺必需键 %q", key)
		}
	}
}

// TestInternalUsageFromResponses_NestedDetailsPreferred 入站侧：嵌套字段优先于旧格式顶层键。
// 两者并存时（真实上游两种都发），嵌套是权威值。
func TestInternalUsageFromResponses_NestedDetailsPreferred(t *testing.T) {
	u := &Usage{
		InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100,
		// 顶层与嵌套故意不一致：嵌套值才是权威
		CacheCreationTokens: 111,
		CacheReadTokens:     222,
		InputTokensDetails:  &InputTokensDetails{CachedTokens: 800, CacheWriteTokens: 300},
		OutputTokensDetails: &OutputTokensDetails{ReasoningTokens: 42},
	}
	iu := internalUsageFromResponses(u)
	if iu.CacheReadTokens != 800 {
		t.Errorf("CacheReadTokens = %d, want 800（嵌套优先于顶层 222）", iu.CacheReadTokens)
	}
	if iu.CacheWriteTokens != 300 {
		t.Errorf("CacheWriteTokens = %d, want 300（嵌套优先于顶层 111）", iu.CacheWriteTokens)
	}
	if iu.ReasoningTokens != 42 {
		t.Errorf("ReasoningTokens = %d, want 42", iu.ReasoningTokens)
	}
}

// TestInternalUsageFromResponses_LegacyTopLevel 旧格式上游（只发顶层键）不能丢。
func TestInternalUsageFromResponses_LegacyTopLevel(t *testing.T) {
	u := &Usage{
		InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100,
		CacheCreationTokens: 250,
		CacheReadTokens:     700,
	}
	iu := internalUsageFromResponses(u)
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

// TestResponsesUsage_NilInputs 两个 helper 都必须容忍 nil。
func TestResponsesUsage_NilInputs(t *testing.T) {
	if got := responsesUsageFromInternal(nil); got != nil {
		t.Errorf("responsesUsageFromInternal(nil) = %v, want nil", got)
	}
	if got := internalUsageFromResponses(nil); got != nil {
		t.Errorf("internalUsageFromResponses(nil) = %v, want nil", got)
	}
}

// TestTranslateStream_CompletedCarriesNestedUsage 端到端：中枢 usage 带 cache/reasoning
// 时，出站 response.completed 的 usage 必须带嵌套 details。
//
// @AI_GUARD: RESPONSES_USAGE_DETAILS_NESTED - 流式终态帧是 Codex 拿 usage 的唯一位置
// @CONSTRAINT: case "done" / done 尾帧有界等待 / case "usage" 三条赋值路径都必须走
//
//	responsesUsageFromInternal。本测试走的是 case "usage"（finish 帧不带 usage、
//	上游只发 delta 后直接给 usage 的退化形态）。
func TestTranslateStream_CompletedCarriesNestedUsage(t *testing.T) {
	// 退化形态：delta → usage-only（无 finish 帧）→ 通道关闭
	events := []schema.InternalStreamEvent{
		{Type: "delta", Data: &schema.InternalStreamChunk{
			ID: "chunk_1", Model: "glm-5.2",
			Choices: []schema.InternalChoice{{Index: 0, Message: schema.InternalMessage{
				Role:    schema.RoleAssistant,
				Content: json.RawMessage(`"ok"`),
			}}},
		}},
		{Type: "usage", Data: &schema.InternalStreamChunk{
			ID: "chunk_2", Model: "glm-5.2",
			Usage: &schema.InternalUsage{
				PromptTokens:     12000,
				CompletionTokens: 400,
				TotalTokens:      12400,
				CacheReadTokens:  10000,
				CacheWriteTokens: 2000,
				ReasoningTokens:  150,
			},
		}},
	}

	sse, _ := runStream(t, events)
	data := lastDataLine(sse, "response.completed")
	if data == "" {
		t.Fatalf("未收到 response.completed\n全部输出:\n%s", sse)
	}

	var ev struct {
		Response struct {
			Usage struct {
				InputTokens        int `json:"input_tokens"`
				OutputTokens       int `json:"output_tokens"`
				TotalTokens        int `json:"total_tokens"`
				InputTokensDetails *struct {
					CachedTokens     int `json:"cached_tokens"`
					CacheWriteTokens int `json:"cache_write_tokens"`
				} `json:"input_tokens_details"`
				OutputTokensDetails *struct {
					ReasoningTokens int `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		t.Fatalf("response.completed 不是合法 JSON: %v\n%s", err, data)
	}
	u := ev.Response.Usage
	if u.InputTokens != 12000 || u.OutputTokens != 400 || u.TotalTokens != 12400 {
		t.Errorf("usage 三键 = %d/%d/%d, want 12000/400/12400",
			u.InputTokens, u.OutputTokens, u.TotalTokens)
	}
	if u.InputTokensDetails == nil {
		t.Fatalf("response.completed 的 usage 缺 input_tokens_details——Codex 会读到 cached=0\n%s", data)
	}
	if u.InputTokensDetails.CachedTokens != 10000 {
		t.Errorf("cached_tokens = %d, want 10000", u.InputTokensDetails.CachedTokens)
	}
	if u.InputTokensDetails.CacheWriteTokens != 2000 {
		t.Errorf("cache_write_tokens = %d, want 2000", u.InputTokensDetails.CacheWriteTokens)
	}
	if u.OutputTokensDetails == nil {
		t.Fatalf("response.completed 的 usage 缺 output_tokens_details——Codex 会读到 reasoning=0\n%s", data)
	}
	if u.OutputTokensDetails.ReasoningTokens != 150 {
		t.Errorf("reasoning_tokens = %d, want 150", u.OutputTokensDetails.ReasoningTokens)
	}
}

// TestTranslateStream_CompletedOmitsEmptyDetails 无 cache/reasoning 时终态帧不得出现
// 空的 details 对象（Option 语义，见上方 guard）。
func TestTranslateStream_CompletedOmitsEmptyDetails(t *testing.T) {
	events := []schema.InternalStreamEvent{
		{Type: "delta", Data: &schema.InternalStreamChunk{
			ID: "c1", Model: "glm-5.2",
			Choices: []schema.InternalChoice{{Index: 0, Message: schema.InternalMessage{
				Role:    schema.RoleAssistant,
				Content: json.RawMessage(`"ok"`),
			}}},
		}},
		{Type: "usage", Data: &schema.InternalStreamChunk{
			ID: "c2", Model: "glm-5.2",
			Usage: &schema.InternalUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110},
		}},
	}
	sse, _ := runStream(t, events)
	data := lastDataLine(sse, "response.completed")
	if data == "" {
		t.Fatalf("未收到 response.completed\n全部输出:\n%s", sse)
	}
	for _, bad := range []string{"input_tokens_details", "output_tokens_details"} {
		var got map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &got); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		var resp map[string]json.RawMessage
		if err := json.Unmarshal(got["response"], &resp); err != nil {
			t.Fatalf("response 不是对象: %v", err)
		}
		var usage map[string]json.RawMessage
		if err := json.Unmarshal(resp["usage"], &usage); err != nil {
			t.Fatalf("usage 不是对象: %v", err)
		}
		if _, ok := usage[bad]; ok {
			t.Errorf("无 %s 数据时仍发出 %q（空对象等于「明确无缓存」，应为缺字段）",
				bad, bad)
		}
	}
}

// TestNoInlineUsageAtLastUsageSites 用 AST 锁住：所有 lastUsage / next 赋值点都必须
// 走 responsesUsageFromInternal，禁止内联 &Usage{...}。
//
// 为什么用 AST：唯一会回归的方式就是把 helper 调用改回手搓字面量——编译期、go vet 零提示，
// 唯一的后果是下一次带缓存的请求 Codex 读到 cached=0，而解析不失败（三键齐全）。
// 与 server/large_body_route_test.go 同一思路：测「这个块里出现了哪些调用」。
//
// @AI_GUARD: RESPONSES_USAGE_DETAILS_NESTED - 与 translator.go 同 guard
func TestNoInlineUsageAtLastUsageSites(t *testing.T) {
	raw, err := os.ReadFile("translator.go")
	if err != nil {
		t.Fatalf("read translator.go: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "translator.go", string(raw), parser.ParseComments)
	if err != nil {
		t.Fatalf("parse translator.go: %v", err)
	}

	targets := map[string]bool{"lastUsage": true, "next": true}
	var inline []string
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || !(as.Tok == token.ASSIGN || as.Tok == token.DEFINE) {
			return true
		}
		// 只关心右侧是 &Usage{...} 字面量的赋值
		_, isUsageLit := as.Rhs[0].(*ast.UnaryExpr)
		if !isUsageLit {
			return true
		}
		ue := as.Rhs[0].(*ast.UnaryExpr)
		if ue.Op != token.AND {
			return true
		}
		cl, ok := ue.X.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if sel, ok := cl.Type.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Usage" {
			return true
		}
		for _, l := range as.Lhs {
			if id, ok := l.(*ast.Ident); ok && targets[id.Name] {
				inline = append(inline, "translator.go:"+fset.Position(cl.Pos()).String())
			}
		}
		return true
	})

	if len(inline) > 0 {
		t.Errorf("lastUsage/next 有 %d 处内联 &Usage{...}，必须改走 responsesUsageFromInternal"+
			"（内联版本漏写嵌套 details 会让 Codex 读到 cached=0 且解析不失败）:\n%s",
			len(inline), joinLines(inline))
	}
}

// TestLastUsageSitesAllUseHelper 正向断言：三个 lastUsage/next 赋值点确实调用了 helper。
// 反向断言（上方）只能防「改回字面量」，本测试防「改成其他错误的构造方式」。
func TestLastUsageSitesAllUseHelper(t *testing.T) {
	raw, err := os.ReadFile("translator.go")
	if err != nil {
		t.Fatalf("read translator.go: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "translator.go", string(raw), parser.ParseComments)
	if err != nil {
		t.Fatalf("parse translator.go: %v", err)
	}

	calls := map[string]int{
		"lastUsage": 0, // case "done" + done 尾帧有界等待
		"next":      0, // case "usage"
	}
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || !(as.Tok == token.ASSIGN || as.Tok == token.DEFINE) || len(as.Lhs) == 0 {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok || !isTarget(id.Name, calls) {
			return true
		}
		// 右侧必须是 responsesUsageFromInternal 调用
		ce, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := ce.Fun.(*ast.Ident)
		if !ok || fn.Name != "responsesUsageFromInternal" {
			return true
		}
		calls[id.Name]++
		return true
	})
	if calls["lastUsage"] != 2 {
		t.Errorf("lastUsage = responsesUsageFromInternal(...) 出现 %d 次, want 2（case \"done\" + 尾帧有界等待）", calls["lastUsage"])
	}
	if calls["next"] != 1 {
		t.Errorf("next := responsesUsageFromInternal(...) 出现 %d 次, want 1（case \"usage\"）", calls["next"])
	}
}

func isTarget(name string, m map[string]int) bool {
	_, ok := m[name]
	return ok
}

func joinLines(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += "\n  "
		}
		out += x
	}
	return out
}

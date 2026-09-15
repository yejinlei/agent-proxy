package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/db"
	"github.com/agent-proxy/agent-proxy/internal/protocol/responses"
	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// TestRootCause_ModelAliasMissing 验证缺失别名已修复
func TestRootCause_ModelAliasMissing(t *testing.T) {
	af := db.DefaultAliases()
	tests := []string{"gpt-4o-mini", "gpt-4o", "o3-mini"}
	for _, model := range tests {
		t.Run(model, func(t *testing.T) {
			target, hit := af.Resolve(model)
			if !hit {
				t.Fatalf("%s 未命中别名 → 透传原值到 Sensenova → 400 (target=%q)", model, target)
			}
			t.Logf("✅ %s → %q", model, target)
		})
	}
}

// TestRootCause_LeakageFields 验证 buildCCRequest 的潜在字段泄露
func TestRootCause_LeakageFields(t *testing.T) {
	t.Run("response_format_text_leak", func(t *testing.T) {
		ir := &schema.InternalRequest{
			Model:          "gpt-4o-mini",
			Messages:       []schema.InternalMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
			Stream:         true,
			ResponseFormat: &schema.InternalResponseFormat{Type: "text"},
		}
		cc := buildCCRequest(ir, "")
		raw, _ := json.Marshal(cc)
		var decoded map[string]interface{}
		json.Unmarshal(raw, &decoded)
		if _, ok := decoded["response_format"]; ok {
			t.Logf("⚠️  response_format={type:text} 泄露到上游 CC → 可能 400 (type='text' 非 CC 合法值)")
		}
	})

	t.Run("stream_options_in_nonstream", func(t *testing.T) {
		ir := &schema.InternalRequest{
			Model:    "gpt-4o-mini",
			Messages: []schema.InternalMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
			Stream:   false,
		}
		cc := buildCCRequest(ir, "")
		raw, _ := json.Marshal(cc)
		var decoded map[string]interface{}
		json.Unmarshal(raw, &decoded)
		if _, ok := decoded["stream_options"]; ok {
			t.Logf("⚠️  非流式请求带 stream_options → 部分上游可能拒绝")
		}
	})
}

// TestEndToEnd_ModelAliasReplacement 验证 gpt-4o-mini 经别名解析后上游收到的真实模型名
func TestEndToEnd_ModelAliasReplacement(t *testing.T) {
	var receivedModel string
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[{"id":"sensenova-6.7-flash-lite"}]}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		json.Unmarshal(body, &req)
		if m, ok := req["model"].(string); ok {
			receivedModel = m
		}
		w.Write([]byte(`{"id":"chatcmpl","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	defer mockServer.Close()

	qg := NewQuickGateway("mock", mockServer.URL, "sk-test",
		[]string{"openai"}, map[string][]string{}, "", "", 30, "", false, 0)
	qg.SetAliasFile(db.DefaultAliases())
	router := qg.Routes()

	body := map[string]any{
		"model":    "gpt-4o-mini",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream":   false,
	}
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	t.Logf("上游收到的 model 名: %q", receivedModel)
	if receivedModel == "gpt-4o-mini" {
		t.Fatalf("gpt-4o-mini 未被替换 → 直接透传（ensureModels 失败或 modelsMap 未填充）")
	}
	t.Logf("✅ gpt-4o-mini 已替换为 %q", receivedModel)
}

// TestDeveloperToSystem 验证 developer→system 映射
func TestDeveloperToSystem(t *testing.T) {
	tr := responses.NewResponsesTranslator()
	ir, err := tr.TranslateRequest(context.Background(), json.RawMessage(`{
		"model":"gpt-4o-mini","input":[
			{"type":"message","role":"developer","content":"Hi"},
			{"type":"message","role":"user","content":"hi"}
		],"stream":false
	}`))
	if err != nil {
		t.Fatal(err)
	}
	cc := buildCCRequest(ir, "")
	raw, _ := json.Marshal(cc)
	var decoded map[string]interface{}
	json.Unmarshal(raw, &decoded)
	msgs := decoded["messages"].([]interface{})
	first := msgs[0].(map[string]interface{})
	if first["role"] != "system" {
		t.Fatalf("developer 未映射为 system: %q", first["role"])
	}
	t.Logf("✅ developer → system")
}

// TestCodex_StreamOptionsAlwaysInjected 验证 stream_options 对任何上游都必须注入。
//
// @AI_GUARD: CC_STREAM_OPTIONS_SENSENOVA - 回归 v0.2.142 的错误归因
// @REASON: 旧测试断言「sensenova 上游必须省略 stream_options」，据说是修复
//   Codex 400 "Invalid request format"。该归因错误：直连实测代理真实 buildCCRequest 输出体，
//   原样发 = 200 但无 usage，加 stream_options = 200 且有 usage，把 system 改成 developer = 400。
//   真凶是 role:"developer"（本函数已 mapRoleToCC 映射成 system），与 stream_options 无关。
//   省略 stream_options 的代价是感诺上游永不回 usage → Codex auto_compact 阈值恒 0 → 历史无界增长。
// @CONSTRAINT: 两个上游都必须带 stream_options.include_usage=true。禁止按 baseURL 分支省略。
func TestCodex_StreamOptionsAlwaysInjected(t *testing.T) {
	content := json.RawMessage(`"hi"`)
	ir := &schema.InternalRequest{
		Model:    "sensenova-6.8-flash-lite",
		Messages: []schema.InternalMessage{{Role: "user", Content: content}},
		Stream:   true,
		Tools: []schema.InternalTool{
			{Type: "function", Function: &schema.InternalFunction{Name: "read", Parameters: map[string]interface{}{}}},
			{Type: "function"}, // nil Function → 空 Tool 槽，应被过滤
		},
	}

	hasStreamOptions := func(raw []byte) bool {
		var decoded map[string]interface{}
		json.Unmarshal(raw, &decoded)
		so, ok := decoded["stream_options"].(map[string]interface{})
		if !ok {
			return false
		}
		return so["include_usage"] == true
	}

	for _, base := range []string{
		"https://token.sensenova.cn/v1",
		"https://api.sensenova.com/v1",
		"https://api.openai.com/v1",
		"https://example.com/v1",
	} {
		raw, _ := json.Marshal(buildCCRequest(ir, base))
		if !hasStreamOptions(raw) {
			t.Fatalf("上游 %s 必须带 stream_options.include_usage=true（流式 usage 的唯一开关），实际: %s", base, raw)
		}
	}
	t.Logf("✅ 所有上游均带 stream_options.include_usage=true")
}

// TestBuildCCRequest_NeverEmitsDeveloperRole 锁定感诺 400 的真凶不会外泄。
//
// @REASON: 感诺对 role:"developer" 一律 400 "inference request is invalid"
//   （glm-5.2 与 sensenova-6.8 均如此），而 system/user/assistant/tool 全部 200。
//   buildCCRequest 第 1411 行写 "system"、mapRoleToCC 把 "developer"→"system"，
//   这里把这个不变量钉住——一旦回归成 developer，就重新变成一条必然 400 的线。
func TestBuildCCRequest_NeverEmitsDeveloperRole(t *testing.T) {
	ir := &schema.InternalRequest{
		Model:        "glm-5.2",
		SystemPrompt: json.RawMessage(`"you are a coding agent"`),
		Messages: []schema.InternalMessage{
			{Role: "developer", Content: json.RawMessage(`"extra dev instructions"`)},
			{Role: "system", Content: json.RawMessage(`"skipped"`)},
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
		Stream: true,
	}

	raw, _ := json.Marshal(buildCCRequest(ir, "https://token.sensenova.cn/v1"))
	var decoded map[string]interface{}
	json.Unmarshal(raw, &decoded)
	msgs, _ := decoded["messages"].([]interface{})
	seen := map[string]bool{}
	for _, m := range msgs {
		role := m.(map[string]interface{})["role"]
		if role == "developer" {
			t.Fatalf("感诺对 role:\"developer\" 一律 400；代理不得外泄该 role: %s", raw)
		}
		seen[role.(string)] = true
	}
	if !seen["system"] {
		t.Fatalf("system 提示必须落到 role:\"system\"（感诺接受）: %s", raw)
	}
	t.Logf("✅ 无 developer role 外泄；role 集合 = %v", seen)
}

// TestBuildCCRequest_EmptyToolSlotFiltered 验证 nil Function 的 Tool 槽不会序列化为 {"type":"","function":null}
func TestBuildCCRequest_EmptyToolSlotFiltered(t *testing.T) {
	ir := &schema.InternalRequest{
		Model:    "sensenova-6.8-flash-lite",
		Messages: []schema.InternalMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []schema.InternalTool{
			{Type: "function"}, // nil Function
			{Type: "function", Function: &schema.InternalFunction{Name: "read"}},
		},
	}
	cc := buildCCRequest(ir, "https://token.sensenova.cn/v1")
	raw, _ := json.Marshal(cc)
	if bytes.Contains(raw, []byte(`"function":null`)) {
		t.Fatalf("不应含空 Tool 槽 function:null: %s", raw)
	}
	if bytes.Contains(raw, []byte(`"type":""`)) {
		t.Fatalf("不应含空 type 字段: %s", raw)
	}
	t.Logf("✅ 空 Tool 槽已过滤, tools=%d", len(cc.Tools))
}

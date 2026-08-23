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
			Model:    "gpt-4o-mini",
			Messages: []schema.InternalMessage{{Role: "user", Content: json.RawMessage(`"hi"`) }},
			Stream:   true,
			ResponseFormat: &schema.InternalResponseFormat{Type: "text"},
		}
		cc := buildCCRequest(ir)
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
			Messages: []schema.InternalMessage{{Role: "user", Content: json.RawMessage(`"hi"`) }},
			Stream:   false,
		}
		cc := buildCCRequest(ir)
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
		"model": "gpt-4o-mini",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream": false,
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
	cc := buildCCRequest(ir)
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

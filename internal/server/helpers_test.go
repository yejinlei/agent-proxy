package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// ── makeTranslationInfo / makePassthroughInfo 防御拷贝 + 模型名注入 ──

func TestMakeTranslationInfo_DefensiveCopy(t *testing.T) {
	shared := &schema.ProviderInfo{
		Name:     "gemini-provider",
		BaseURL:  "https://example.com",
		APIToken: "secret",
		Version:  "gemini",
	}

	callInfo := makeTranslationInfo(shared, "gemini-1.5-flash")

	if callInfo.Name != "gemini-1.5-flash" {
		t.Errorf("Name: got %q, want %q", callInfo.Name, "gemini-1.5-flash")
	}
	if callInfo.Version != "gemini" {
		t.Errorf("Version: got %q, want %q", callInfo.Version, "gemini")
	}
	if callInfo.BaseURL != "https://example.com" {
		t.Errorf("BaseURL mismatch")
	}
	if callInfo.APIToken != "secret" {
		t.Errorf("APIToken mismatch")
	}

	callInfo.Name = "mutated"
	if shared.Name != "gemini-provider" {
		t.Errorf("shared info was mutated; got %q", shared.Name)
	}
}

func TestMakePassthroughInfo(t *testing.T) {
	shared := &schema.ProviderInfo{
		Name:     "openai-provider",
		BaseURL:  "https://openai.com",
		APIToken: "tok",
		Version:  "openai",
	}

	callInfo := makePassthroughInfo(shared, "gpt-4o")

	if callInfo.Name != "gpt-4o" {
		t.Errorf("Name: got %q, want %q", callInfo.Name, "gpt-4o")
	}
	if callInfo.Version != "" {
		t.Errorf("Version should be empty, got %q", callInfo.Version)
	}

	callInfo.Name = "mutated"
	if shared.Name != "openai-provider" {
		t.Errorf("shared info was mutated")
	}
}

// ── quickExtractModel 单测 ──

func TestQuickExtractModel(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		body     string
		want     string
	}{
		{
			name:     "chatcompletion with stream",
			protocol: "chatcompletion",
			body:     `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			want:     "gpt-4o",
		},
		{
			name:     "anthropic messages",
			protocol: "anthropic",
			body:     `{"model":"claude-3-opus","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`,
			want:     "claude-3-opus",
		},
		{
			name:     "responses api",
			protocol: "responses",
			body:     `{"model":"gpt-4o-mini","input":"hello","stream":false}`,
			want:     "gpt-4o-mini",
		},
		{
			name:     "chatcompletion missing model",
			protocol: "chatcompletion",
			body:     `{"messages":[{"role":"user","content":"hi"}]}`,
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := quickExtractModel(json.RawMessage(tt.body), tt.protocol)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// ── quickDetectStream 单测 ──

func TestQuickDetectStream(t *testing.T) {
	tests := []struct {
		body string
		want bool
	}{
		{`{"stream":true}`, true},
		{`{"stream":false}`, false},
		{`{"model":"x","stream":true}`, true},
		{`{"model":"x"}`, false},
		{`{}`, false},
	}
	for _, tt := range tests {
		got := quickDetectStream(json.RawMessage(tt.body))
		if got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.body, got, tt.want)
		}
	}
}

// ── makeQuickPassthroughInfo ──

func TestMakeQuickPassthroughInfo(t *testing.T) {
	shared := &schema.ProviderInfo{
		Name:     "openai-proxy",
		BaseURL:  "https://base.com",
		APIToken: "tok",
	}

	info := makeQuickPassthroughInfo(shared, "gpt-4")
	if info.Name != "gpt-4" {
		t.Errorf("Name: got %q", info.Name)
	}
	info.Name = "mutated"
	if shared.Name != "openai-proxy" {
		t.Errorf("shared mutated")
	}
}

// ── sendError 响应格式 (quick) ──

func TestQuickGateway_sendError(t *testing.T) {
	w := httptest.NewRecorder()
	q := &QuickGateway{proxyName: "test"}
	q.sendError(w, 400, "missing_model", "model required")

	if w.Code != 400 {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	err, ok := out["error"]
	if !ok {
		t.Fatalf("no error key in response")
	}
	// sendError stores the error *type* under "type" and the HTTP status code
	// (as a string) under "code".
	if err.(map[string]interface{})["type"] != "missing_model" {
		t.Errorf("error type mismatch: got %v", err.(map[string]interface{})["type"])
	}
	if err.(map[string]interface{})["code"] != "400" {
		t.Errorf("error code mismatch: got %v", err.(map[string]interface{})["code"])
	}
}

// ── quickExtractModel & quickDetectStream 空 body 防御 ──

func TestQuickExtractModel_EmptyBody(t *testing.T) {
	got := quickExtractModel(json.RawMessage(`{}`), "chatcompletion")
	if got != "" {
		t.Errorf("expected empty model, got %q", got)
	}
}

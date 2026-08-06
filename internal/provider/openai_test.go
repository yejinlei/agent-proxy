package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

func newGeminiClient() *GeminiClient {
	return NewGeminiClient("gemini", "https://api.example.com", 30)
}

// ── BuildURL 单测 ──

func TestGeminiClient_BuildURL_NonStream(t *testing.T) {
	c := newGeminiClient()
	info := &schema.ProviderInfo{APIToken: "tok123"}

	got := c.BuildURL(info, "gemini-1.5-flash", false)
	want := "https://api.example.com/v1/models/gemini-1.5-flash:generateContent"
	if got != want {
		t.Errorf("non-stream: got %q, want %q", got, want)
	}
}

func TestGeminiClient_BuildURL_Stream(t *testing.T) {
	c := newGeminiClient()
	info := &schema.ProviderInfo{APIToken: "tok123"}

	got := c.BuildURL(info, "gemini-1.5-pro", true)
	want := "https://api.example.com/v1/models/gemini-1.5-pro:generateContent?key=tok123"
	if got != want {
		t.Errorf("stream: got %q, want %q", got, want)
	}
}

// ── Call 发送 URL 单测（验证 info.Name 兜底模型名）──

func TestGeminiClient_Call_URL(t *testing.T) {
	c := newGeminiClient()
	servedURL := ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servedURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`))
	}))
	defer ts.Close()

	c.baseURL = ts.URL

	info := &schema.ProviderInfo{
		Name:     "gemini-1.5-flash",
		BaseURL:  ts.URL,
		APIToken: "secret",
	}
	req := json.RawMessage(`{"contents":[{"parts":[{"text":"hello"}]}]}`)

	_, _, err := c.Call(context.Background(), req, info)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}

	wantPath := "/v1/models/gemini-1.5-flash:generateContent"
	if servedURL != wantPath {
		t.Errorf("request URL: got %q, want %q", servedURL, wantPath)
	}
}

// info.Name 为空时，URL 中模型名应为空串
func TestGeminiClient_Call_URL_EmptyName(t *testing.T) {
	c := newGeminiClient()
	servedURL := ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servedURL = r.URL.String()
		w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	c.baseURL = ts.URL

	info := &schema.ProviderInfo{APIToken: "tok"}
	req := json.RawMessage(`{}`)

	_, _, err := c.Call(context.Background(), req, info)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}

	if servedURL != "/v1/models/:generateContent" {
		t.Errorf("empty name URL: got %q, want %q", servedURL, "/v1/models/:generateContent")
	}
}

// ── CallStream 发送 URL 单测 ──

func TestGeminiClient_CallStream_URL(t *testing.T) {
	c := newGeminiClient()
	servedURL := ""
	reqID := ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servedURL = r.URL.String()
		reqID = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`event: message
data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}
`))
	}))
	defer ts.Close()
	c.baseURL = ts.URL

	info := &schema.ProviderInfo{
		Name:     "gemini-1.5-pro",
		BaseURL:  ts.URL,
		APIToken: "secret",
	}
	req := json.RawMessage(`{"contents":[{"parts":[{"text":"hello"}]}]}`)

	lines, _, err := c.CallStream(context.Background(), req, info)
	if err != nil {
		t.Fatalf("CallStream failed: %v", err)
	}

	select {
	case _, ok := <-lines:
		if !ok {
			t.Fatal("stream closed without data")
	}
	case <-time.After(2 * time.Second):
		t.Fatal("stream data not received within 2s")
	}

	wantPath := "/v1/models/gemini-1.5-pro:generateContent?key=secret"
	if servedURL != wantPath {
		t.Errorf("stream URL: got %q, want %q", servedURL, wantPath)
	}
	if reqID != "text/event-stream" {
		t.Errorf("Accept header: got %q, want %q", reqID, "text/event-stream")
	}
}

// ── DefaultHeaders ──

func TestGeminiClient_DefaultHeaders(t *testing.T) {
	c := newGeminiClient()
	info := &schema.ProviderInfo{APIToken: "bearer123"}

	h := c.DefaultHeaders(info)
	if h.Get("Authorization") != "Bearer bearer123" {
		t.Errorf("Authorization: got %q, want %q", h.Get("Authorization"), "Bearer bearer123")
	}
	if h.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type: got %q", h.Get("Content-Type"))
	}
}

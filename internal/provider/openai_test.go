package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// ── 重试策略 ──

func TestRetryableStatus(t *testing.T) {
	retryable := []int{408, 429, 500, 502, 503, 504}
	for _, code := range retryable {
		if !retryableStatus(code) {
			t.Errorf("retryableStatus(%d) = false, want true", code)
		}
	}
	// 400/401/403/404/413 是请求本身的问题，重发必然同样失败
	notRetryable := []int{400, 401, 403, 404, 413}
	for _, code := range notRetryable {
		if retryableStatus(code) {
			t.Errorf("retryableStatus(%d) = true, want false", code)
		}
	}
}

func TestRetryDelay(t *testing.T) {
	if retryDelay(0) != 0 {
		t.Errorf("retryDelay(0) = %v, want 0", retryDelay(0))
	}
	if d := retryDelay(1); d != 100*time.Millisecond {
		t.Errorf("retryDelay(1) = %v, want 100ms", d)
	}
	if d := retryDelay(2); d != 400*time.Millisecond {
		t.Errorf("retryDelay(2) = %v, want 400ms", d)
	}
}

func TestRetryAfterDelay(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
		want time.Duration
	}{
		{name: "无 Retry-After 用下限", h: http.Header{}, want: retryAfterFloor},
		{name: "整数秒", h: http.Header{"Retry-After": []string{"7"}}, want: 7 * time.Second},
		{name: "小于下限取下限", h: http.Header{"Retry-After": []string{"1"}}, want: retryAfterFloor},
		{name: "超过上限取上限", h: http.Header{"Retry-After": []string{"600"}}, want: retryAfterCap},
		{name: "非法值回退下限", h: http.Header{"Retry-After": []string{"soon"}}, want: retryAfterFloor},
		{name: "带空白的整数秒", h: http.Header{"Retry-After": []string{"  12  "}}, want: 12 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryAfterDelay(tc.h); got != tc.want {
				t.Errorf("retryAfterDelay = %v, want %v", got, tc.want)
			}
		})
	}

	// HTTP-date 形式。注意 time.Parse 在 UTC 下会把 RFC1123 的 Z 区格式化成字面
	// "UTC"，而 http.ParseTime 只认 "GMT"——所以下面的字符串手写 GMT 结尾。
	futureUTC := time.Now().UTC().Add(20 * time.Second)
	h := http.Header{"Retry-After": []string{strings.Replace(futureUTC.Format(time.RFC1123), " UTC", " GMT", 1)}}
	if got := retryAfterDelay(h); got < 18*time.Second || got > 22*time.Second {
		t.Errorf("retryAfterDelay(RFC1123 GMT) = %v, want ~20s (got=%s)", got, h.Get("Retry-After"))
	}

	// 带偏移量的日期（部分上游用 +0000 而非 GMT）
	h = http.Header{"Retry-After": []string{futureUTC.Format(time.RFC1123Z)}}
	if got := retryAfterDelay(h); got < 18*time.Second || got > 22*time.Second {
		t.Errorf("retryAfterDelay(RFC1123Z) = %v, want ~20s (got=%s)", got, h.Get("Retry-After"))
	}
}

func TestParseRetryAfter_InvalidDateFallsThrough(t *testing.T) {
	// 非法日期不能把结果变成一个大数值（time.Parse 对 RFC1123 的 Z 区容错会返回 zero time + nil error）
	if got := parseRetryAfter("not-a-date"); got != 0 {
		t.Errorf("parseRetryAfter(invalid) = %v, want 0", got)
	}
	if got := parseRetryAfter("Mon, 08 Sep 2026 20:52:31 CST"); got != 0 {
		t.Errorf("parseRetryAfter(non-GMT literal) = %v, want 0", got)
	}
	if got := parseRetryAfter("-5"); got != -5*time.Second {
		t.Errorf("parseRetryAfter(-5) = %v, want -5s (下限由调用方套)", got)
	}
}

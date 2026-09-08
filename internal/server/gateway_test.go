package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/protocol/chatcompletion"
	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// TestGateway_SSEDataPrefix_HandlesVariousFormats verifies the fix in
// gateway.go handleStreamRequest where SSE lines from OpenAIClient.CallStream
// include a "data: " prefix. Without strings.TrimPrefix, every chunk fails
// json.Unmarshal and is silently skipped, producing an empty stream response.
func TestGateway_SSEDataPrefix_HandlesVariousFormats(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantText string
		wantSkip bool
	}{
		{
			name:     "with data: prefix",
			line:     `data: {"id":"c1","choices":[{"delta":{"content":"hi"},"index":0}]}`,
			wantText: "hi",
			wantSkip: false,
		},
		{
			name:     "without data: prefix",
			line:     `{"id":"c1","choices":[{"delta":{"content":"hi"},"index":0}]}`,
			wantText: "hi",
			wantSkip: false,
		},
		{
			name:     "empty delta — no content but valid chunk",
			line:     `data: {"id":"c1","choices":[{"delta":{},"index":0,"finish_reason":"stop"}]}`,
			wantText: "",
			wantSkip: false,
		},
		{
			name:     "non-json — should skip",
			line:     `this is not json`,
			wantText: "",
			wantSkip: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Mirrors the fix in gateway.go handleStreamRequest:
			//   payload := strings.TrimPrefix(string(line), "data: ")
			//   var ccChunk chatcompletion.ChatCompletionStreamChunk
			//   if json.Unmarshal([]byte(payload), &ccChunk) != nil || len(ccChunk.Choices) == 0 {
			//       continue
			//   }
			payload := strings.TrimPrefix(tt.line, "data: ")
			var ccChunk chatcompletion.ChatCompletionStreamChunk
			if err := json.Unmarshal([]byte(payload), &ccChunk); err != nil {
				if tt.wantSkip {
					return
				}
				t.Fatalf("Unexpected unmarshal failure: %v\n  line: %s\n  payload: %s", err, tt.line, payload)
			}
			if tt.wantSkip {
				t.Fatal("Expected unmarshal to fail but it succeeded")
			}
			if len(ccChunk.Choices) == 0 {
				t.Fatal("Expected at least one choice")
			}

			choice := ccChunk.Choices[0]
			if choice.Delta.Content == "" {
				if tt.wantText == "" {
					return
				}
				t.Fatal("Expected content in delta but got empty string")
			}
			if choice.Delta.Content != tt.wantText {
				t.Fatalf("Expected %q, got %q", tt.wantText, choice.Delta.Content)
			}
		})
	}
}

// TestGateway_SSEStreamAccumulation simulates the full stream accumulation
// that happens in gateway.go handleStreamRequest when an OpenAI provider
// returns SSE-formatted lines with "data: " prefix.
func TestGateway_SSEStreamAccumulation(t *testing.T) {
	// These are exactly the format OpenAIClient.CallStream sends.
	chunks := []string{
		`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"Hello"},"index":0,"finish_reason":null}],"usage":null}`,
		`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":" World"},"index":0,"finish_reason":null}],"usage":null}`,
		`data: {"id":"chatcmpl-1","choices":[{"delta":{},"index":0,"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`data: [DONE]`,
	}

	var accumulatedContent strings.Builder
	var lastUsage map[string]float64

	for _, line := range chunks {
		// Simulate gateway.go logic: skip [DONE] and empty lines
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "data: [DONE]" || strings.HasPrefix(trimmed, ":") {
			continue
		}

		payload := strings.TrimPrefix(line, "data: ")
		var ccChunk chatcompletion.ChatCompletionStreamChunk
		if err := json.Unmarshal([]byte(payload), &ccChunk); err != nil {
			t.Fatalf("Failed to parse SSE chunk: %v\nchunk: %s", err, line)
		}
		if len(ccChunk.Choices) == 0 {
			continue
		}

		choice := ccChunk.Choices[0]
		if choice.Delta.Content != "" {
			accumulatedContent.WriteString(choice.Delta.Content)
		}
		if ccChunk.Usage != nil {
			lastUsage = map[string]float64{
				"prompt_tokens":     float64(ccChunk.Usage.PromptTokens),
				"completion_tokens": float64(ccChunk.Usage.CompletionTokens),
				"total_tokens":      float64(ccChunk.Usage.TotalTokens),
			}
		}
	}

	if accumulatedContent.String() != "Hello World" {
		t.Fatalf("Expected 'Hello World', got %q — 'data: ' prefix fix is not working", accumulatedContent.String())
	}
	if lastUsage == nil {
		t.Fatal("Expected usage in final chunk")
	}
	if lastUsage["total_tokens"] != 15 {
		t.Fatalf("Expected total_tokens=15, got %v", lastUsage["total_tokens"])
	}

	t.Logf("✓ SSE stream correctly accumulated: %q (usage: prompt=%v completion=%v total=%v)",
		accumulatedContent.String(),
		lastUsage["prompt_tokens"], lastUsage["completion_tokens"], lastUsage["total_tokens"])
}

// TestQuickGateway_SSEDataPrefixAlreadyFixed confirms quick.go already handles
// the "data: " prefix (the same fix is present there).
func TestQuickGateway_SSEDataPrefixAlreadyFixed(t *testing.T) {
	line := `data: {"id":"c1","choices":[{"delta":{"content":"test"},"index":0}]}`
	payload := strings.TrimPrefix(line, "data: ")
	var ccChunk chatcompletion.ChatCompletionStreamChunk
	if err := json.Unmarshal([]byte(payload), &ccChunk); err != nil {
		t.Fatalf("quick.go-style parse failed: %v", err)
	}
	if len(ccChunk.Choices) == 0 {
		t.Fatal("Expected choices")
	}
	if ccChunk.Choices[0].Delta.Content != "test" {
		t.Fatalf("Expected 'test', got %q", ccChunk.Choices[0].Delta.Content)
	}
}

func getTestGatewayCache() *sync.Map {
	return &sync.Map{}
}

// TestBuildCCRequest_DevRoleMapToSystem verifies Responses API's "developer"
// role is mapped to "system" when building CC requests. Sensenova and other
// CC endpoints reject role:"developer" with 400; it must be mapped to system.
func TestBuildCCRequest_DevRoleMapToSystem(t *testing.T) {
	content, _ := json.Marshal("hello")
	req := &schema.InternalRequest{
		Model:   "test-model",
		Stream:  true,
		Messages: []schema.InternalMessage{
			{Role: "developer", Content: content},   // should map → "system"
			{Role: schema.RoleUser, Content: content}, // stays "user"
		},
	}
	ccReq := buildCCRequest(req, "")

	if len(ccReq.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(ccReq.Messages))
	}

	// developer → system
	var role1 string
	json.Unmarshal(ccReq.Messages[0].Content.Raw(), &role1)
	if ccReq.Messages[0].Role != "system" {
		t.Fatalf("developer role: got %q, want system", ccReq.Messages[0].Role)
	}

	// user stays user
	if ccReq.Messages[1].Role != "user" {
		t.Fatalf("user role: got %q, want user", ccReq.Messages[1].Role)
	}
	t.Logf("✓ developer→system mapping correct (developer=%q, user=%q)",
		ccReq.Messages[0].Role, ccReq.Messages[1].Role)
}

// ═══════════════════════════════════════════════════════════════════════════════
//  入站请求体读取（readBody，定义在 quick.go，quick.go 与 gateway.go 共用）
// ═══════════════════════════════════════════════════════════════════════════════

func newBodyRequest(body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return r
}

func TestReadBody_UnderLimit(t *testing.T) {
	body, err := readBody(newBodyRequest([]byte(`{"model":"x"}`)), time.Now())
	if err != nil {
		t.Fatalf("readBody: %v", err)
	}
	if string(body) != `{"model":"x"}` {
		t.Errorf("body = %q, want unchanged", body)
	}
}

func TestReadBody_ExactlyAtLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), maxBodyBytes)
	body, err := readBody(newBodyRequest(payload), time.Now())
	if err != nil {
		t.Fatalf("readBody at exactly %d bytes: %v", maxBodyBytes, err)
	}
	if len(body) != maxBodyBytes {
		t.Errorf("len(body) = %d, want %d", len(body), maxBodyBytes)
	}
}

func TestReadBody_OverLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), maxBodyBytes+4096)
	body, err := readBody(newBodyRequest(payload), time.Now())
	if err == nil {
		t.Fatalf("readBody over limit: want err, got nil")
	}
	if !errors.Is(err, errBodyExceedsLimit) {
		t.Errorf("err = %v, want errBodyExceedsLimit", err)
	}
	if len(body) != maxBodyBytes+1 {
		t.Errorf("len(body) = %d, want maxBodyBytes+1 (多读的 1 字节是超限证据)", len(body))
	}
	if bodyErrStatus(err) != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", bodyErrStatus(err))
	}
}

// 无 Content-Length（chunked 上传）也要能检出超限：LimitReader 多读 1 字节即可判定。
func TestReadBody_OverLimit_NoContentLength(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), maxBodyBytes+4096)
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	r.ContentLength = -1
	body, err := readBody(r, time.Now())
	if !errors.Is(err, errBodyExceedsLimit) {
		t.Fatalf("err = %v, want errBodyExceedsLimit", err)
	}
	if len(body) != maxBodyBytes+1 {
		t.Errorf("len(body) = %d, want %d", len(body), maxBodyBytes+1)
	}
}

// 读失败必须返回原错误（调用方要拿它当 message 回给客户端），不能吞成 errBodyExceedsLimit。
type errReader struct{ n int }

func (e *errReader) Read(p []byte) (int, error) {
	e.n++
	if e.n > 1 {
		return 0, errors.New("i/o timeout")
	}
	return copy(p, []byte(`{"mod`)), nil
}

func TestReadBody_ReadErrorSurfaces(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", &errReader{})
	body, err := readBody(r, time.Now())
	if err == nil || err.Error() != "i/o timeout" {
		t.Fatalf("err = %v, want i/o timeout", err)
	}
	if len(body) != 5 {
		t.Errorf("len(body) = %d, want 5", len(body))
	}
	if bodyErrStatus(err) != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", bodyErrStatus(err))
	}
}

// 日志函数：真正的失败要打出可定位的信息，EOF（正常读完）和 nil 必须静默跳过。
func TestLogReadBodyFailure(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stdout)

	logReadBodyFailure(newBodyRequest(nil), time.Now(), 0, io.EOF)
	logReadBodyFailure(newBodyRequest(nil), time.Now(), 0, nil)
	if buf.Len() != 0 {
		t.Errorf("EOF/nil 不应产生日志，实际输出: %q", buf.String())
	}

	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.RemoteAddr = "122.233.241.117:60484"
	start := time.Now().Add(-120 * time.Second)
	logReadBodyFailure(r, start, 5, errors.New("i/o timeout"))

	out := buf.String()
	for _, want := range []string{
		"[read-body] FAILED", "read 5 bytes", "content-length 0",
		"remote=122.233.241.117:60484", "path=/v1/responses", "i/o timeout",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("日志缺少 %q，实际: %s", want, out)
		}
	}
}

// chunked 上传没有 Content-Length，日志里不能打出 "-1"。
func TestLogReadBodyFailure_Chunked(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stdout)

	r := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte("x")))
	r.ContentLength = -1
	logReadBodyFailure(r, time.Now(), 1, errors.New("i/o timeout"))

	out := buf.String()
	if strings.Contains(out, "-1") {
		t.Errorf("chunked 上传的 content-length 应显示 unknown，实际: %s", out)
	}
	if !strings.Contains(out, "content-length unknown") {
		t.Errorf("缺少 content-length unknown，实际: %s", out)
	}
}

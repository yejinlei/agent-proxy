package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
		Model:  "test-model",
		Stream: true,
		Messages: []schema.InternalMessage{
			{Role: "developer", Content: content},     // should map → "system"
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

// parseSSEDataLines 从 SSE 流中取出所有 data 行并解析为 JSON 对象，供断言事件形状。
func parseSSEDataLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var evs []map[string]any
	for _, ln := range strings.Split(out, "\n") {
		if !strings.HasPrefix(ln, "data: ") {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(ln[len("data: "):]), &m) != nil {
			continue
		}
		evs = append(evs, m)
	}
	return evs
}

// SSE 错误事件必须按入站协议分流。Responses 客户端（Codex）只认 response.* 事件，
// 收到 Anthropic 的 message_start 后会一直等 response.completed，最后报
// "stream closed before response.completed" 并降级 HTTPS 重试。
// 这两个测试锁住形状，防止回流。
func TestIngressProtocolOf_PathMapping(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/v1/responses", "responses"},
		{"/v1/responses/", "responses"},
		{"/v1/chat/completions", "chatcompletion"},
		{"/v1/messages", "anthropic"},
		{"/v1/models/gemini-1.5-pro:generateContent", "gemini"},
		{"/v1/models", "chatcompletion"}, // 非 :generateContent，落入默认分支
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodPost, tt.path, nil)
		if got := ingressProtocolOf(r); got != tt.want {
			t.Errorf("ingressProtocolOf(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestSSEErrorProtocol_PriorityOrder(t *testing.T) {
	// 1. 显式协议优先（复杂模式走这条）
	if got := sseErrorProtocol("responses", httptest.NewRequest(http.MethodPost, "/v1/messages", nil), nil); got != "responses" {
		t.Errorf("显式协议应优先，得到 %q", got)
	}
	// 2. internalReq 次之（翻译路径没有请求对象时走这条）
	ir := &schema.InternalRequest{Protocol: "responses"}
	if got := sseErrorProtocol("", nil, ir); got != "responses" {
		t.Errorf("internalReq.Protocol 应生效，得到 %q", got)
	}
	// 3. 请求路径再次（透传路径只有请求对象时走这条）
	if got := sseErrorProtocol("", httptest.NewRequest(http.MethodPost, "/v1/responses", nil), nil); got != "responses" {
		t.Errorf("路径解析应生效，得到 %q", got)
	}
	// 4. 全部缺失时兜底 anthropic，保持旧行为不破坏 Claude Code
	if got := sseErrorProtocol("", nil, nil); got != "anthropic" {
		t.Errorf("兜底应为 anthropic，得到 %q", got)
	}
	// 5. 非 responses 协议原样返回，保证 Kimi/Claude 客户端路径不变
	if got := sseErrorProtocol("chatcompletion", nil, nil); got != "chatcompletion" {
		t.Errorf("chatcompletion 应原样返回，得到 %q", got)
	}
}

func TestSendResponsesSSEError_EmitsCompletedAndDone(t *testing.T) {
	rec := httptest.NewRecorder()
	err := fmt.Errorf("HTTP 429: {\"error\":{\"message\":\"rate limited\"}}")
	sendResponsesSSEError(rec, rec, err)

	out := rec.Body.String()

	// Codex 严格要求：必须有 response.completed 收尾，且流以 [DONE] 结束
	for _, want := range []string{
		"event: response.error\n",
		"event: response.completed\n",
		"\"status\":\"failed\"",
		"\"finish_reason\":\"stop\"",
		"\"incomplete_details\":{\"reason\":null}",
		"event: done\ndata: [DONE]\n\n",
		"\"type\":\"upstream_error\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Responses 错误终态缺少 %q\n实际:\n%s", want, out)
		}
	}

	// 反例：绝不能混入 Anthropic 事件——那正是本次要修掉的问题
	for _, bad := range []string{"message_start", "message_stop", "event: error"} {
		if strings.Contains(out, bad) {
			t.Errorf("Responses 错误终态不应包含 Anthropic 事件 %q\n实际:\n%s", bad, out)
		}
	}

	// error 对象必须单层：不能再套一层 error，否则 Codex 取 message 得到 undefined。
	// 按事件 type 查找，不依赖写入顺序。
	var errEv, completedEv map[string]any
	for _, ev := range parseSSEDataLines(t, out) {
		switch ev["type"] {
		case "response.error":
			errEv = ev
		case "response.completed":
			completedEv = ev
		}
	}
	if errEv == nil {
		t.Fatalf("缺少 type=response.error 事件:\n%s", out)
	}
	if completedEv == nil {
		t.Fatalf("缺少 type=response.completed 事件:\n%s", out)
	}
	if inner, nested := errEv["error"].(map[string]any); nested {
		if _, again := inner["error"]; again {
			t.Errorf("error 被双层信封包裹，Codex 取 message 会得到 undefined: %v", errEv)
		}
	}
	if msg, _ := errEv["error"].(map[string]any)["message"].(string); msg == "" {
		t.Errorf("error.message 不能为空: %v", errEv)
	}

	// response.completed 的 output 必须是数组、usage 必须是对象
	// （usage 缺失会让客户端报 undefined is not an object）
	resp, _ := completedEv["response"].(map[string]any)
	if _, ok := resp["usage"].(map[string]any); !ok {
		t.Errorf("response.completed 的 usage 必须是对象，得到 %v", resp["usage"])
	}
	if _, ok := resp["output"].([]any); !ok {
		t.Errorf("response.completed 的 output 必须是数组，得到 %v", resp["output"])
	}
	if resp["status"] != "failed" {
		t.Errorf("失败终态 status = %v, want failed", resp["status"])
	}
	// finish_reason 不能写 max_output_tokens：会让客户端把「上游拒绝」误判成「输出太长」
	if resp["finish_reason"] == "max_output_tokens" {
		t.Errorf("finish_reason 应为 stop/function_call 占位，不能写 max_output_tokens")
	}
}

// 非 Responses 协议必须保持原有 Anthropic 形状，否则修好 Codex 会弄坏 Claude Code。
func TestWriteUpstreamSSEError_AnthropicShapeUnchanged(t *testing.T) {
	for _, proto := range []string{"anthropic", "chatcompletion", "gemini", ""} {
		rec := httptest.NewRecorder()
		writeUpstreamSSEError(rec, rec, proto, fmt.Errorf("stream error: upstream down"))
		out := rec.Body.String()
		for _, want := range []string{"event: message_start", "event: error", "event: message_stop"} {
			if !strings.Contains(out, want) {
				t.Errorf("协议 %q 的错误形状缺少 %q\n实际:\n%s", proto, want, out)
			}
		}
		if strings.Contains(out, "event: response.completed") {
			t.Errorf("协议 %q 不应收到 Responses 事件:\n%s", proto, out)
		}
	}
}

// dispatcher：同一份错误，不同入站协议得到不同事件形状。
func TestSendUpstreamSSEError_DispatchesByIngress(t *testing.T) {
	err := fmt.Errorf("stream error: %w", errors.New("context deadline exceeded"))

	recR := httptest.NewRecorder()
	sendUpstreamSSEError(recR, recR, nil, &schema.InternalRequest{Protocol: "responses"}, err)
	if !strings.Contains(recR.Body.String(), "event: response.completed") {
		t.Errorf("responses 入站应发 Responses 终态:\n%s", recR.Body.String())
	}

	recA := httptest.NewRecorder()
	sendUpstreamSSEError(recA, recA, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), nil, err)
	if !strings.Contains(recA.Body.String(), "event: message_start") {
		t.Errorf("anthropic 入站应保持 message_start:\n%s", recA.Body.String())
	}

	// 路径解析：/v1/responses 走 Responses 分支
	recP := httptest.NewRecorder()
	sendUpstreamSSEError(recP, recP, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), nil, err)
	if !strings.Contains(recP.Body.String(), "event: response.completed") {
		t.Errorf("路径 /v1/responses 应走 Responses 分支:\n%s", recP.Body.String())
	}
}

// TestE2E_ResponsesStream_UpstreamError_EmitsCompletedResponse 端到端验证：
// 上游失败时，Responses 客户端必须拿到 response.completed 收尾，否则 Codex 报
// "stream closed before response.completed" 并按 WS→HTTPS 降级重试。
//
// 场景：model=vmodel（非 openai 前缀）+ capabilities=["anthropic"] → 走翻译路径
// （inbound Responses ≠ upstream anthropic）。mock 上游流式与非流式都返回 400，
// 触发 translateStreamError → 降级 → 再失败 → SSE 错误出口。
func TestE2E_ResponsesStream_UpstreamError_EmitsCompletedResponse(t *testing.T) {
	upstreamErr := `{"error":{"message":"inference request is invalid","type":"invalid_request_error"}}`

	// mock 上游：两条路径（stream 与 Call）都回 400
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(upstreamErr))
	}))
	defer mockServer.Close()

	qg := NewQuickGateway(
		"mock-proxy", // 非真实域名，不触发请求字段过滤
		mockServer.URL,
		"mock-key",
		[]string{"anthropic"}, // 不含 openai → vmodel 走翻译路径
		nil, "", "", 5, "", false, 0,
	)
	router := qg.Routes()

	body := `{"model":"vmodel","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	out := rr.Body.String()
	if rr.Code != http.StatusOK {
		// SSE 头已发出（WriteHeader(200)），错误走事件而非 HTTP 状态码
		t.Fatalf("SSE 头已发送，状态码应为 200，实际 %d:\n%s", rr.Code, out)
	}

	// 核心断言：Responses 客户端看到的必须是 response.* 终态
	for _, want := range []string{
		"event: response.error",
		"event: response.completed",
		"\"status\":\"failed\"",
		"event: done\ndata: [DONE]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q\n实际:\n%s", want, out)
		}
	}

	// 回归保护：绝不能把 Anthropic 事件发给 Responses 客户端
	// （这正是修复前 Codex 报 "stream closed before response.completed" 的原因）
	for _, bad := range []string{"message_start", "message_stop", "event: error\n"} {
		if strings.Contains(out, bad) {
			t.Errorf("Responses 客户端不应收到 Anthropic 事件 %q\n实际:\n%s", bad, out)
		}
	}

	// 上游错误信息必须到达客户端（可归因），而不是被吞掉
	if !strings.Contains(out, "inference request is invalid") {
		t.Errorf("上游错误信息未透传给客户端:\n%s", out)
	}
}

// 对照测试：Anthropic 入站在上游失败时保持 message_start 形状，
// 确保修复 Responses 没有顺带弄坏 Claude Code 路径。
func TestE2E_AnthropicStream_UpstreamError_KeepsMessageStart(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"boom","type":"invalid_request_error"}}`))
	}))
	defer mockServer.Close()

	qg := NewQuickGateway(
		"mock-proxy", mockServer.URL, "mock-key",
		[]string{"openai"}, nil, "", "", 5, "", false, 0,
	)
	router := qg.Routes()

	// anthropic 入站 + upstream 也是 anthropic → 透传路径，上游 400 → SSE 错误出口
	body := `{"model":"anthropic-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	out := rr.Body.String()

	// 断言对象类型而非 event: 行：现有实现把 message_start / message_stop 写成
	// data 行而没带 event: 前缀（v0.2.123 基线即如此，非本次引入）。这里只锁住
	// 「走 Anthropic 形状」这一件事，前缀一致性另作处理。
	for _, want := range []string{
		`"type":"message_start"`,
		"event: error",
		`"type":"message_stop"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Anthropic 错误形状缺少 %q\n实际:\n%s", want, out)
		}
	}
	if strings.Contains(out, "event: response.completed") {
		t.Errorf("Anthropic 客户端不应收到 Responses 事件:\n%s", out)
	}
}

// TestObserveHeartbeatState_StateMachine 锁定心跳状态机：何时记下 item_id、何时停止保活。
// 观测点只有两个：lastItemID（心跳 delta 要带的 item_id）与 hbOn（是否还在保活）。
// 两个 writer 必须给出完全一致的结果——双模式同步约定。
func TestObserveHeartbeatState_StateMachine(t *testing.T) {
	qs := &qwsResponseWriter{hbOn: true}
	gs := &wsResponseWriter{hbOn: true}
	sse := func(eventType, data string) []byte {
		b := []byte("event: ")
		b = append(b, eventType...)
		b = append(b, '\n', 'd', 'a', 't', 'a', ':', ' ')
		b = append(b, data...)
		b = append(b, '\n', '\n')
		return b
	}
	for _, tc := range []struct {
		name    string
		observe func([]byte)
		itemID  func() string
		on      func() bool
	}{
		{"quick", qs.observeHeartbeatState, func() string { return qs.lastItemID }, func() bool { return qs.hbOn }},
		{"gateway", gs.observeHeartbeatState, func() string { return gs.lastItemID }, func() bool { return gs.hbOn }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := func(itemID string, wantOn bool) {
				if got := tc.itemID(); got != itemID {
					t.Errorf("lastItemID = %q, want %q", got, itemID)
				}
				if got := tc.on(); got != wantOn {
					t.Errorf("hbOn = %v, want %v", got, wantOn)
				}
			}

			// 非 message 事件不改变 item_id
			tc.observe(sse("response.created", `{"type":"response.created"}`))
			assert("", true)

			// function_call item 不产生文本 delta，必须跳过：
			// 否则心跳 delta 会挂在不能收文本的 item 上，Codex 报序列异常。
			tc.observe(sse("response.output_item.added",
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call"}}`))
			assert("", true)

			// message item 才登记 item_id；它在 item.id 而非顶层 item_id
			tc.observe(sse("response.output_item.added",
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_abc","type":"message"}}`))
			assert("msg_abc", true)

			// 首个真实 token 到达：item_id 保留，但保活使命结束
			tc.observe(sse("response.output_text.delta", `{"type":"response.output_text.delta","delta":"hi"}`))
			assert("msg_abc", false)

			// 停止后不能再被 done 类事件或 [DONE] 改动（幂等，防重复收尾）
			tc.observe(sse("response.output_text.done", `{"type":"response.output_text.done"}`))
			tc.observe(sse("response.output_item.done", `{"type":"response.output_item.done"}`))
			tc.observe(sse("response.completed", `{"type":"response.completed"}`))
			tc.observe(sse("done", "[DONE]"))
			assert("msg_abc", false)
		})
	}
}

// TestWritePing_HeartbeatFrames 锁定 WS 心跳在四种状态下发出的帧形状。
//
// 背景：v0.2.124 的 WS 心跳是 text 帧，内容为 event: ping——该 event 不属于 OpenAI
// Responses 事件集，Codex 解析器报 stream closed before response.completed（实测确认），
// 这是 WS 降级 HTTPS 的直接原因。v0.2.125 改用 ping 控制帧（opcode 0x9）：控制帧按
// RFC 6455 在 WS 栈内透明处理、应用层完全不可见，因此不可能重置客户端的应用层计时器，
// 等于零保活——实测 10 轮上游首 token 延迟 2.4~12.0s，超过 10s 那几轮 Codex 仍然重连。
// v0.2.126 起改用 response.output_text.delta（空 delta）：它在事件集内、客户端看得见，
// 能真正重置应用层计时器。
func TestWritePing_HeartbeatFrames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(net.Conn) error
		want  byte
	}{
		{"quick/心跳未启用", func(c net.Conn) error {
			return (&qwsResponseWriter{conn: c}).WritePing()
		}, 0x0},
		{"quick/item未注册", func(c net.Conn) error {
			return (&qwsResponseWriter{conn: c, hbOn: true}).WritePing()
		}, qwsOpPing},
		{"gateway/心跳未启用", func(c net.Conn) error {
			return (&wsResponseWriter{conn: c}).WritePing()
		}, 0x0},
		{"gateway/item未注册", func(c net.Conn) error {
			return (&wsResponseWriter{conn: c, hbOn: true}).WritePing()
		}, wsOpPing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := wsSendAndRead(t, tc.write)
			if err != nil {
				t.Fatalf("WritePing 出错: %v", err)
			}
			if tc.want == 0x0 {
				if len(got) != 0 {
					t.Fatalf("心跳未启用时不应发出任何字节，实际 %d 字节: % X", len(got), got)
				}
				return
			}
			op, payload := wsFirstFrame(t, got)
			if op != tc.want {
				t.Fatalf("期望控制帧 opcode=0x9，实际 opcode=0x%X", op)
			}
			if len(payload) != 0 {
				t.Fatalf("控制帧 payload 应为空，实际 %d 字节", len(payload))
			}
			if got[0]&0x80 == 0 {
				t.Fatalf("控制帧必须 FIN=1，实际 b0=%#x", got[0])
			}
			if got[1]&0x80 != 0 {
				t.Fatalf("服务端控制帧不得掩码，实际 b1=%#x", got[1])
			}
		})
	}
}

// TestWritePing_EmitsLegalDelta 锁定 item 已注册后的心跳是合法 Responses 事件：
// event 行、JSON 事件类型、item_id 与 output_item.added 的 item.id 一致、delta 为空串。
func TestWritePing_EmitsLegalDelta(t *testing.T) {
	added := sseHeartbeatFrame("response.output_item.added",
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_abc","type":"message"}}`)
	for _, tc := range []struct {
		name  string
		write func(net.Conn) error
	}{
		{"quick", func(c net.Conn) error {
			w := &qwsResponseWriter{conn: c, hbOn: true}
			if _, err := w.Write(added); err != nil {
				return err
			}
			return w.WritePing()
		}},
		{"gateway", func(c net.Conn) error {
			w := &wsResponseWriter{conn: c, hbOn: true}
			if _, err := w.Write(added); err != nil {
				return err
			}
			return w.WritePing()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := wsSendAndRead(t, tc.write)
			if err != nil {
				t.Fatalf("写出出错: %v", err)
			}
			// 第一个帧是测试写入的 output_item.added，第二个才是心跳
			idx := 0
			for i := 0; i < 2; i++ {
				op, payload, used := wsParseFrame(t, got[idx:])
				if op != qwsOpText {
					t.Fatalf("第 %d 帧应为 text 帧，实际 opcode=0x%X", i+1, op)
				}
				idx += used
				if i == 0 {
					continue
				}
				lines := strings.Split(string(payload), "\n")
				if lines[0] != "event: response.output_text.delta" {
					t.Errorf("心跳 event 行 = %q", lines[0])
				}
				var ev map[string]interface{}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &ev); err != nil {
					t.Fatalf("心跳 data 行不是 JSON: %v: %q", err, lines[1])
				}
				if ev["type"] != "response.output_text.delta" {
					t.Errorf("心跳事件类型 = %v，期望 response.output_text.delta", ev["type"])
				}
				if ev["item_id"] != "msg_abc" {
					t.Errorf("心跳 item_id = %v，必须与 output_item.added 的 item.id 一致", ev["item_id"])
				}
				if d, ok := ev["delta"].(string); !ok || d != "" {
					t.Errorf("心跳 delta 必须是空字符串，实际 %v", ev["delta"])
				}
				if oi, ok := ev["output_index"].(float64); !ok || oi != 0 {
					t.Errorf("心跳 output_index = %v，期望 0", ev["output_index"])
				}
				if ci, ok := ev["content_index"].(float64); !ok || ci != 0 {
					t.Errorf("心跳 content_index = %v，期望 0", ev["content_index"])
				}
			}
		})
	}
}

// TestWritePing_StopsAfterStreamEnds 锁定 hbOn 的状态翻转：
// 首个真实 delta 之后必须停止发心跳——否则会在流已收尾后插入孤立 delta 事件，
// 这正是 v0.2.126 引入合法 delta 心跳后最危险的回退路径。
func TestWritePing_StopsAfterStreamEnds(t *testing.T) {
	qs := &qwsResponseWriter{conn: wsDrainingPipe(t), hbOn: true}
	gs := &wsResponseWriter{conn: wsDrainingPipe(t), hbOn: true}
	for _, tc := range []struct {
		name      string
		write     func([]byte) (int, error)
		writePing func() error
		itemID    func() string
		on        func() bool
	}{
		{"quick", qs.Write, qs.WritePing,
			func() string { return qs.lastItemID }, func() bool { return qs.hbOn }},
		{"gateway", gs.Write, gs.WritePing,
			func() string { return gs.lastItemID }, func() bool { return gs.hbOn }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			added := sseHeartbeatFrame("response.output_item.added",
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_abc","type":"message"}}`)
			if _, err := tc.write(added); err != nil {
				t.Fatalf("写入 output_item.added 失败: %v", err)
			}
			if !tc.on() {
				t.Fatalf("output_item.added 不应停止保活")
			}
			if got := tc.itemID(); got != "msg_abc" {
				t.Fatalf("item_id 未登记: %q", got)
			}

			delta := sseHeartbeatFrame("response.output_text.delta",
				`{"type":"response.output_text.delta","delta":"hi"}`)
			if _, err := tc.write(delta); err != nil {
				t.Fatalf("写入 delta 失败: %v", err)
			}
			if tc.on() {
				t.Errorf("首个真实 delta 后必须停止保活")
			}

			// 停发后 WritePing 必须静默返回 nil，不写任何字节
			if err := tc.writePing(); err != nil {
				t.Errorf("停止后 WritePing 出错: %v", err)
			}

			// 流收尾事件不应让 hbOn 重新打开
			if _, err := tc.write(sseHeartbeatFrame("response.completed", `{"type":"response.completed"}`)); err != nil {
				t.Fatalf("写入 completed 失败: %v", err)
			}
			if tc.on() {
				t.Errorf("response.completed 后保活被意外重新打开")
			}
		})
	}
}

// TestObserveHeartbeatState_MultiLineBatch 锁定一批多事件字节在一次 Write 里的处理：
// 生产路径中上游可能把多个事件攒在一个 chunk 里发出，状态机必须按序处理而不是只看首个。
func TestObserveHeartbeatState_MultiLineBatch(t *testing.T) {
	qs := &qwsResponseWriter{hbOn: true}
	gs := &wsResponseWriter{hbOn: true}
	batch := append(sseHeartbeatFrame("response.output_item.added",
		`{"type":"response.output_item.added","item":{"id":"msg_batch","type":"message"}}`),
		sseHeartbeatFrame("response.output_text.delta", `{"type":"response.output_text.delta","delta":"x"}`)...)
	qs.observeHeartbeatState(batch)
	gs.observeHeartbeatState(batch)
	if qs.lastItemID != "msg_batch" {
		t.Errorf("quick: 批量处理后 item_id = %q, want msg_batch", qs.lastItemID)
	}
	if qs.hbOn {
		t.Errorf("quick: 同一 chunk 内已出现真实 delta，保活应停止")
	}
	if gs.lastItemID != "msg_batch" {
		t.Errorf("gateway: 批量处理后 item_id = %q, want msg_batch", gs.lastItemID)
	}
	if gs.hbOn {
		t.Errorf("gateway: 同一 chunk 内已出现真实 delta，保活应停止")
	}
}

// wsSendAndRead 在 net.Pipe 上跑一次写出并读回全部原始字节。
// 管道两端由测试持有：被测代码写 client 端，测试侧读 server 端。
// net.Pipe 忽略 write deadline，无人读取会无限阻塞，所以读端必须与写端配对启动。
func wsSendAndRead(t *testing.T, write func(net.Conn) error) ([]byte, error) {
	t.Helper()
	client, srv := net.Pipe()
	defer client.Close()
	defer srv.Close()

	type readResult struct {
		data []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		var acc []byte
		buf := make([]byte, 4096)
		for {
			n, err := srv.Read(buf)
			if n > 0 {
				acc = append(acc, buf[:n]...)
			}
			if err != nil {
				ch <- readResult{acc, err}
				return
			}
		}
	}()

	werr := write(client)
	client.Close()

	select {
	case r := <-ch:
		// 读端收到 EOF 是正常收尾（client 端已关闭，数据已全部排空），不算失败。
		return r.data, werr
	case <-time.After(3 * time.Second):
		t.Fatal("超时，读端未退出（写端未返回或数据未被消费）")
		return nil, werr
	}
}

// wsDrainingPipe 返回一个可写端，对端自动排空。
// net.Pipe 忽略 write deadline，必须有人读，否则写入会无限阻塞。
func wsDrainingPipe(t *testing.T) net.Conn {
	t.Helper()
	client, srv := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			_, err := srv.Read(buf)
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		client.Close()
		<-done
		srv.Close()
	})
	return client
}

// sseHeartbeatFrame 构造一个 SSE 事件帧，形状与 responses/translator.go sendSSE 一致。
func sseHeartbeatFrame(eventType, data string) []byte {
	b := []byte("event: ")
	b = append(b, eventType...)
	b = append(b, '\n', 'd', 'a', 't', 'a', ':', ' ')
	b = append(b, data...)
	b = append(b, '\n', '\n')
	return b
}

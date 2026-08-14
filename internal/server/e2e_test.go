package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// buildRequestHelper creates a POST request with JSON body
func buildRequest(url string, body map[string]any) *http.Request {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestQuickGateway_E2E_ChatCompletion_Passthrough(t *testing.T) {
	// This test reuses the mock server + gateway setup from TestQuickGateway_E2E_AllProtocols
	// but is isolated so each subtest can run independently.
	t.Run("non_stream_passthrough", func(t *testing.T) {
		qg := setupMockGateway(t)
		defer qg.mockServer.Close()

		body := map[string]any{
			"model": "gpt-4o-mini",
			"messages": []map[string]string{
				{"role": "user", "content": "hi"},
			},
			"stream": false,
		}
		req := buildRequest("/v1/chat/completions", body)
		rr := httptest.NewRecorder()
		qg.router.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("invalid JSON response: %v: %s", err, rr.Body.String())
		}
		choices, ok := resp["choices"].([]any)
		if !ok || len(choices) == 0 {
			t.Fatal("expected choices array")
		}
		choice, ok := choices[0].(map[string]any)
		if !ok {
			t.Fatal("expected choice object")
		}
		msg, ok := choice["message"].(map[string]any)
		if !ok {
			t.Fatal("expected message object")
		}
		content, _ := msg["content"].(string)
		if content != "Hello World" {
			t.Fatalf("expected 'Hello World', got %q", content)
		}
		t.Logf("✓ CC passthrough non-stream: content=%q", content)
	})

	t.Run("stream_passthrough", func(t *testing.T) {
		qg := setupMockGateway(t)
		defer qg.mockServer.Close()

		body := map[string]any{
			"model": "gpt-4o-mini",
			"messages": []map[string]string{
				{"role": "user", "content": "hi"},
			},
			"stream": true,
		}
		req := buildRequest("/v1/chat/completions", body)
		rr := httptest.NewRecorder()
		qg.router.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "Hello") {
			t.Fatalf("expected stream to contain 'Hello', got: %s", rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), " World") {
			t.Fatalf("expected stream to contain 'World', got: %s", rr.Body.String())
		}
		t.Logf("✓ CC passthrough stream: %q", rr.Body.String()[:100])
	})
}

func TestQuickGateway_E2E_Anthropic_Translation(t *testing.T) {
	t.Run("non_stream", func(t *testing.T) {
		qg := setupMockGateway(t)
		defer qg.mockServer.Close()

		body := map[string]any{
			"model": "gpt-4o-mini",
			"messages": []map[string]string{
				{"role": "user", "content": "hi"},
			},
			"max_tokens": 100,
		}
		req := buildRequest("/v1/messages", body)
		rr := httptest.NewRecorder()
		qg.router.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("invalid JSON: %v: %s", err, rr.Body.String())
		}
		// Anthropic format: content is array of blocks
		content, ok := resp["content"].([]any)
		if !ok {
			t.Fatalf("expected content array in Anthropic response, got type %T: %s", resp["content"], rr.Body.String())
		}
		var texts []string
		for _, block := range content {
			b, _ := block.(map[string]any)
			if b["type"] == "text" {
				if txt, _ := b["text"].(string); txt != "" {
					texts = append(texts, txt)
				}
			}
		}
		if len(texts) == 0 || texts[0] != "Hello World" {
			t.Fatalf("expected 'Hello World' in content blocks, got %v", texts)
		}
		t.Logf("✓ Anthropic translation non-stream: %v", texts)
	})

	t.Run("stream", func(t *testing.T) {
		qg := setupMockGateway(t)
		defer qg.mockServer.Close()

		body := map[string]any{
			"model": "gpt-4o-mini",
			"messages": []map[string]string{
				{"role": "user", "content": "hi"},
			},
			"max_tokens": 100,
			"stream":     true,
		}
		req := buildRequest("/v1/messages", body)
		rr := httptest.NewRecorder()
		qg.router.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		bodyStr := rr.Body.String()
		// Anthropic SSE uses "event: content_block_delta" + "data: {...}"
		if !strings.Contains(bodyStr, "Hello") {
			t.Fatalf("expected stream to contain 'Hello', got:\n%s", bodyStr)
		}
		if !strings.Contains(bodyStr, " World") {
			t.Fatalf("expected stream to contain 'World', got:\n%s", bodyStr)
		}
		// Verify Anthropic SSE format (event: lines)
		if !strings.Contains(bodyStr, "content_block_delta") {
			t.Fatalf("expected Anthropic SSE event 'content_block_delta', got:\n%s", bodyStr)
		}
		t.Logf("✓ Anthropic translation stream (first 200 chars): %q", bodyStr[:min(200, len(bodyStr))])
	})
}

func TestQuickGateway_E2E_Gemini_Translation(t *testing.T) {
	t.Run("non_stream", func(t *testing.T) {
		qg := setupMockGateway(t)
		defer qg.mockServer.Close()

		// Gemini request format
		body := map[string]any{
			"contents": []map[string]any{
				{
					"role": "user",
					"parts": []map[string]string{
						{"text": "hi"},
					},
				},
			},
		}
		req := buildRequest("/v1/models/gpt-4o-mini:generateContent", body)
		rr := httptest.NewRecorder()
		qg.router.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("invalid JSON: %v: %s", err, rr.Body.String())
		}
		candidates, ok := resp["candidates"].([]any)
		if !ok || len(candidates) == 0 {
			t.Fatalf("expected candidates array, got: %s", rr.Body.String())
		}
		candidate, _ := candidates[0].(map[string]any)
		content, _ := candidate["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		if len(parts) == 0 {
			t.Fatalf("expected parts, got: %s", rr.Body.String())
		}
		part, _ := parts[0].(map[string]any)
		text, _ := part["text"].(string)
		if text != "Hello World" {
			t.Fatalf("expected 'Hello World', got %q: %s", text, rr.Body.String())
		}
		t.Logf("✓ Gemini translation non-stream: content=%q", text)
	})

	t.Run("stream", func(t *testing.T) {
		qg := setupMockGateway(t)
		defer qg.mockServer.Close()

		body := map[string]any{
			"contents": []map[string]any{
				{
					"role": "user",
					"parts": []map[string]string{
						{"text": "hi"},
					},
				},
			},
			"stream": true,
		}
		req := buildRequest("/v1/models/gpt-4o-mini:generateContent", body)
		rr := httptest.NewRecorder()
		qg.router.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		bodyStr := rr.Body.String()
		if !strings.Contains(bodyStr, "Hello") {
			t.Fatalf("expected stream to contain 'Hello', got:\n%s", bodyStr)
		}
		if !strings.Contains(bodyStr, " World") {
			t.Fatalf("expected stream to contain 'World', got:\n%s", bodyStr)
		}
		t.Logf("✓ Gemini translation stream (first 200 chars): %q", bodyStr[:min(200, len(bodyStr))])
	})
}

func TestQuickGateway_E2E_Responses_Translation(t *testing.T) {
	t.Run("non_stream", func(t *testing.T) {
		qg := setupMockGateway(t)
		defer qg.mockServer.Close()

		body := map[string]any{
			"model": "gpt-4o-mini",
			"input": "hi",
		}
		req := buildRequest("/v1/responses", body)
		rr := httptest.NewRecorder()
		qg.router.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("invalid JSON: %v: %s", err, rr.Body.String())
		}
		output, ok := resp["output"].([]any)
		if !ok {
			t.Fatalf("expected output array, got type %T: %s", resp["output"], rr.Body.String())
		}
		var foundContent string
		for _, item := range output {
			itemMap, _ := item.(map[string]any)
			if itemMap["type"] == "message" {
				if content, ok := itemMap["content"].([]any); ok {
					for _, block := range content {
						b, _ := block.(map[string]any)
						if b["type"] == "output_text" {
							if txt, _ := b["text"].(string); txt != "" {
								foundContent += txt
							}
						}
					}
				}
			}
		}
		if foundContent != "Hello World" {
			t.Fatalf("expected 'Hello World' in output, got %q: %s", foundContent, rr.Body.String())
		}
		t.Logf("✓ Responses translation non-stream: content=%q", foundContent)
	})

	t.Run("stream", func(t *testing.T) {
		qg := setupMockGateway(t)
		defer qg.mockServer.Close()

		body := map[string]any{
			"model":  "gpt-4o-mini",
			"input":  "hi",
			"stream": true,
		}
		req := buildRequest("/v1/responses", body)
		rr := httptest.NewRecorder()
		qg.router.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		bodyStr := rr.Body.String()
		if !strings.Contains(bodyStr, "Hello") {
			t.Fatalf("expected stream to contain 'Hello', got:\n%s", bodyStr)
		}
		if !strings.Contains(bodyStr, " World") {
			t.Fatalf("expected stream to contain 'World', got:\n%s", bodyStr)
		}
		t.Logf("✓ Responses translation stream (first 200 chars): %q", bodyStr[:min(200, len(bodyStr))])
	})
}

// ── Shared helper ──
type mockGateway struct {
	mockServer *httptest.Server
	router     interface {
		ServeHTTP(http.ResponseWriter, *http.Request)
	}
}

func setupMockGateway(t *testing.T) *mockGateway {
	t.Helper()

	nonStreamResp := `{
		"id":"chatcmpl-mock",
		"object":"chat.completion",
		"created":1700000000,
		"model":"gpt-4o-mini",
		"choices":[{"index":0,"message":{"role":"assistant","content":"Hello World"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}
	}`

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		var req map[string]any
		if json.Unmarshal(body, &req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		stream, _ := req["stream"].(bool)

		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")

			chunks := []string{
				`data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
				`data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
				`data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":" World"},"finish_reason":null}]}`,
				`data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
				`data: [DONE]`,
			}
			for _, chunk := range chunks {
				w.Write([]byte(chunk + "\n\n"))
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(nonStreamResp))
		}
	}))

	qg := NewQuickGateway(
		"mock-proxy",
		mockServer.URL,
		"mock-key",
		[]string{"openai"},
		nil,
		30,
		"", false, 0,
	)
	router := qg.Routes()

	return &mockGateway{
		mockServer: mockServer,
		router:     router,
	}
}

// TestTimingLogsSlowUpstream 测试慢速上游的耗时统计日志
// 验证 -vv 模式下 [request]/[handler]/[upstream] 三类耗时日志在慢速上游场景下正确输出
func TestTimingLogsSlowUpstream(t *testing.T) {
	// 捕获日志输出
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(io.Discard)

	// 模拟慢速上游（2 秒延迟）的 mock server
	nonStreamResp := `{
		"id":"chatcmpl-mock",
		"object":"chat.completion",
		"created":1700000000,
		"model":"gpt-4o-mini",
		"choices":[{"index":0,"message":{"role":"assistant","content":"Hello Slow World"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}
	}`

	slowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		var req map[string]any
		if json.Unmarshal(body, &req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		stream, _ := req["stream"].(bool)

		// 模拟 2 秒慢速上游
		time.Sleep(2 * time.Second)

		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			// 流式响应：每 500ms 发一个 chunk，模拟慢速流式
			chunks := []string{
				`data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
				`data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"Slow"},"finish_reason":null}]}`,
				`data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
				`data: [DONE]`,
			}
			for _, chunk := range chunks {
				w.Write([]byte(chunk + "\n\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				time.Sleep(500 * time.Millisecond)
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(nonStreamResp))
		}
	}))
	defer slowServer.Close()

	// 用 verboseLevel=2 创建 QuickGateway
	qg := NewQuickGateway(
		"mock-proxy",
		slowServer.URL,
		"mock-key",
		[]string{"openai"},
		nil,
		30,
		"", false, 2,
	)
	router := qg.Routes()

	t.Run("non_stream_slow", func(t *testing.T) {
		body := map[string]any{
			"model": "gpt-4o-mini",
			"messages": []map[string]string{
				{"role": "user", "content": "hi"},
			},
			"stream": false,
		}
		req := buildRequest("/v1/chat/completions", body)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}

		logs := logBuf.String()
		// 验证三类耗时日志都存在
		if !strings.Contains(logs, "[request] total") {
			t.Error("missing [request] total timing log")
		}
		if !strings.Contains(logs, "[handler]") {
			t.Error("missing [handler] timing log")
		}
		if !strings.Contains(logs, "[upstream] Call") {
			t.Error("missing [upstream] timing log")
		}
		t.Logf("Slow non-stream logs:\n%s", logs)
	})

	t.Run("stream_slow", func(t *testing.T) {
		logBuf.Reset()

		body := map[string]any{
			"model": "gpt-4o-mini",
			"messages": []map[string]string{
				{"role": "user", "content": "hi"},
			},
			"stream": true,
		}
		req := buildRequest("/v1/chat/completions", body)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}

		logs := logBuf.String()
		if !strings.Contains(logs, "[request] total") {
			t.Error("missing [request] total timing log")
		}
		if !strings.Contains(logs, "[handler]") {
			t.Error("missing [handler] timing log")
		}
		// auto-probe 首次请求走 passthrough-nonstream-as-sse，使用 Call 而非 CallStream
		if !strings.Contains(logs, "[upstream] Call") {
			t.Error("missing [upstream] timing log")
		}
		t.Logf("Slow stream (auto-probe) logs:\n%s", logs)
	})
}

// TestHeartbeatDuringLongUpstreamCall 验证上游长时间响应时心跳是否正常发送
// 模拟 5 秒上游延迟（测试环境限制，比 70 秒短但足够验证心跳机制）
// 关键断言：
//  1. SSE 响应中包含心跳行（event: ping\ndata: {"type":"ping"}）
//  2. 心跳在 Call/CallStream 期间发送，而非在 Call 之后
//  3. 日志中存在 [heartbeat] started（两阶段心跳 >= 2 次 started）
//  4. 日志中心跳次数 > 0
func TestHeartbeatDuringLongUpstreamCall(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(io.Discard)

	nonStreamResp := `{
		"id":"chatcmpl-mock",
		"object":"chat.completion",
		"created":1700000000,
		"model":"gpt-4o-mini",
		"choices":[{"index":0,"message":{"role":"assistant","content":"Hello After Delay"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}
	}`

	// 模拟 5 秒延迟的上游（>3 个心跳周期，每个 500ms）
	delay := 5 * time.Second

	slowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		var req map[string]any
		if json.Unmarshal(body, &req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		stream, _ := req["stream"].(bool)

		// 关键：在响应前等待 5 秒
		time.Sleep(delay)

		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			chunks := []string{
				`data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
				`data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"Hello After Delay"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
				`data: [DONE]`,
			}
			for _, chunk := range chunks {
				w.Write([]byte(chunk + "\n\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(nonStreamResp))
		}
	}))
	defer slowServer.Close()

	qg := NewQuickGateway(
		"mock-proxy",
		slowServer.URL,
		"mock-key",
		[]string{"openai"},
		nil,
		30,
		"", false, 2, // verboseLevel=2
	)
	router := qg.Routes()

	t.Run("translation_stream_with_heartbeat", func(t *testing.T) {
		logBuf.Reset()

		// 翻译路径：Anthropic → ChatCompletion，流式 → handleStreamRequest
		// 这会走 handleStreamRequest，在 CallStream 阻塞等待上游时发送心跳
		body := map[string]any{
			"model":      "gpt-4o-mini",
			"max_tokens": 1024,
			"stream":     true,
			"messages": []map[string]string{
				{"role": "user", "content": "hi"},
			},
		}
		req := buildRequest("/v1/messages", body)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}

		logs := logBuf.String()

		// 验证心跳启动日志存在（两阶段心跳，至少 2 次 started）
		heartbeatStartedCount := strings.Count(logs, "[heartbeat] started")
		if heartbeatStartedCount < 2 {
			t.Errorf("expected >= 2 heartbeat started logs (two-phase), got %d", heartbeatStartedCount)
		}

		// 验证心跳发送次数 > 0（阶段 2 覆盖流处理等待期）
		heartbeatCount := strings.Count(logs, "[heartbeat] sent #")
		if heartbeatCount == 0 {
			t.Error("no heartbeat sent during upstream call")
		}
		t.Logf("Heartbeat count during translation CallStream: %d", heartbeatCount)

		// 验证 SSE 响应包含心跳行
		responseBody := rr.Body.String()
		if !strings.Contains(responseBody, `event: ping`) {
			t.Error("SSE response does not contain heartbeat line 'event: ping'")
		}

		// 验证最终 SSE 响应有效（Anthropic 协议使用 data: 前缀）
		if !strings.Contains(responseBody, "data: ") {
			t.Error("SSE response missing data: prefix")
		}

		if !strings.Contains(logs, "[request] total") {
			t.Error("missing [request] total log")
		}

		t.Logf("=== Full logs (translation stream) ===\n%s", logs)
		t.Logf("=== Response body (len=%d) ===\n%s", len(responseBody), responseBody)
	})

	t.Run("passthrough_stream_with_heartbeat", func(t *testing.T) {
		logBuf.Reset()

		body := map[string]any{
			"model": "gpt-4o-mini",
			"messages": []map[string]string{
				{"role": "user", "content": "hi"},
			},
			"stream": true, // 流式请求
		}
		req := buildRequest("/v1/chat/completions", body)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}

		logs := logBuf.String()

		// auto-probe: 首次流式请求走 passthrough-nonstream-as-sse（探测真实协议）
		// 验证心跳启动日志存在
		heartbeatStartedCount := strings.Count(logs, "[heartbeat] started")
		if heartbeatStartedCount < 1 {
			t.Errorf("expected >= 1 heartbeat started log, got %d", heartbeatStartedCount)
		}

		// 验证心跳发送次数
		heartbeatCount := strings.Count(logs, "[heartbeat] sent #")
		if heartbeatCount == 0 {
			t.Error("no heartbeat sent during upstream call")
		}
		t.Logf("Heartbeat count during auto-probe: %d", heartbeatCount)

		// 验证响应包含心跳
		responseBody := rr.Body.String()
		if !strings.Contains(responseBody, `event: ping`) {
			t.Error("SSE response does not contain heartbeat line 'event: ping'")
		}

		t.Logf("=== Full logs (stream auto-probe) ===\n%s", logs)
		t.Logf("=== Response body (len=%d) ===\n%s", len(responseBody), responseBody)
	})

	t.Logf("=== Completed all heartbeat tests (delay=%v) ===", delay)
}

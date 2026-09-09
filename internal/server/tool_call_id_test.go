package server

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// TestBuildCCRequest_PreservesToolCallID 验证 role:tool 消息的 tool_call_id 不被丢弃。
//
// 背景：Responses 的 function_call_output 在中枢里带 ToolCallID，
// 但 buildCCRequest 曾漏拷该字段，导致上游收到「有调用无配对」的工具结果，
// 模型误判文件未读取并停止发起工具调用（Codex 一直不回读）。
// 见 gateway.go 的 @AI_GUARD: CC_TOOL_CALL_ID_LINKAGE。
func TestBuildCCRequest_PreservesToolCallID(t *testing.T) {
	ir := &schema.InternalRequest{
		Model: "sensenova-6.8-flash-lite",
		Messages: []schema.InternalMessage{
			{Role: "user", Content: json.RawMessage(`"Read _batch_out.txt"`)},
			{
				Role:    "assistant",
				Content: json.RawMessage(`""`),
				ToolCalls: []schema.InternalToolCall{
					{
						ID:   "call_1",
						Type: "function",
						Function: struct {
							Name         string          `json:"name"`
							Arguments    string          `json:"arguments"`
							RawArguments json.RawMessage `json:"-"`
						}{
							Name:         "exec_command",
							Arguments:    `{"cmd":"cat _batch_out.txt"}`,
							RawArguments: json.RawMessage(`{"cmd":"cat _batch_out.txt"}`),
						},
					},
				},
			},
			{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"line1\nline2"`)},
		},
	}

	cc := buildCCRequest(ir, "https://token.sensenova.cn/v1")
	raw, err := json.Marshal(cc)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	msgs, ok := decoded["messages"].([]interface{})
	if !ok || len(msgs) != 3 {
		t.Fatalf("messages 应为 3 条，实际 %d: %s", len(msgs), raw)
	}

	toolMsg, ok := msgs[2].(map[string]interface{})
	if !ok {
		t.Fatalf("第 3 条不是对象: %s", raw)
	}
	if toolMsg["role"] != "tool" {
		t.Fatalf("role 应为 tool，实际 %v", toolMsg["role"])
	}
	got, _ := toolMsg["tool_call_id"].(string)
	if got != "call_1" {
		t.Fatalf("tool_call_id 丢失：上游会收到悬空工具结果，Codex 因此不回读。实际=%q raw=%s", got, raw)
	}

	// assistant 侧的 tool_calls[].id 必须与 tool 消息的 tool_call_id 同值，才能配对
	assistantMsg, ok := msgs[1].(map[string]interface{})
	if !ok {
		t.Fatalf("第 2 条不是对象: %s", raw)
	}
	calls, ok := assistantMsg["tool_calls"].([]interface{})
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls 应为 1 条: %s", raw)
	}
	callID, _ := calls[0].(map[string]interface{})["id"].(string)
	if callID != got {
		t.Fatalf("调用 id %q 与结果 tool_call_id %q 不一致", callID, got)
	}

	t.Logf("✅ tool_call_id 已保留并配对: %s", raw)
}

// TestBuildCCRequest_WithoutToolCallIDOmitted 验证非 tool 消息不会多出空 tool_call_id 字段。
func TestBuildCCRequest_WithoutToolCallIDOmitted(t *testing.T) {
	ir := &schema.InternalRequest{
		Model:    "sensenova-6.8-flash-lite",
		Messages: []schema.InternalMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	raw, _ := json.Marshal(buildCCRequest(ir, "https://token.sensenova.cn/v1"))
	if bytes.Contains(raw, []byte(`tool_call_id`)) {
		t.Fatalf("无工具结果时不应输出 tool_call_id: %s", raw)
	}
	t.Logf("✅ 无工具结果时 tool_call_id 已省略")
}

// TestBuildCCRequest_OrphanDetection 覆盖 buildCCRequest 的悬空工具结果检查（orphaned_tool_results）：
// 配对完整时 orphaned_tool_results=0；tool_call_id 丢失（v0.2.121 修复前的形态）时必须 >0。
// 这条断言保证下一轮回归时悬空工具结果在日志里立即可见，不必再靠 A/B 对照实验定位。
func TestBuildCCRequest_OrphanDetection(t *testing.T) {
	tc := schema.InternalToolCall{
		ID:   "call_1",
		Type: "function",
		Function: struct {
			Name         string          `json:"name"`
			Arguments    string          `json:"arguments"`
			RawArguments json.RawMessage `json:"-"`
		}{Name: "exec_command", Arguments: `{"cmd":"cat f.txt"}`},
	}

	cases := []struct {
		name       string
		msgs       []schema.InternalMessage
		wantOrphan int
	}{
		{
			name: "配对完整",
			msgs: []schema.InternalMessage{
				{Role: "user", Content: json.RawMessage(`"read f.txt"`)},
				{Role: "assistant", ToolCalls: []schema.InternalToolCall{tc}},
				{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"ok"`)},
			},
			wantOrphan: 0,
		},
		{
			name: "tool_call_id 丢失即孤儿",
			msgs: []schema.InternalMessage{
				{Role: "user", Content: json.RawMessage(`"read f.txt"`)},
				{Role: "assistant", ToolCalls: []schema.InternalToolCall{tc}},
				{Role: "tool", Content: json.RawMessage(`"ok"`)}, // 缺 ToolCallID → 悬空
			},
			wantOrphan: 1,
		},
		{
			name: "id 对不上也是孤儿",
			msgs: []schema.InternalMessage{
				{Role: "user", Content: json.RawMessage(`"read f.txt"`)},
				{Role: "assistant", ToolCalls: []schema.InternalToolCall{tc}},
				{Role: "tool", ToolCallID: "call_999", Content: json.RawMessage(`"ok"`)},
			},
			wantOrphan: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := new(bytes.Buffer)
			orig := log.Writer()
			log.SetOutput(buf)
			defer log.SetOutput(orig)
			buf.Reset()

			buildCCRequest(&schema.InternalRequest{Model: "vmodel", Messages: c.msgs}, "")
			out := buf.String()

			const key = "orphaned_tool_results="
			i := strings.Index(out, key)
			if i < 0 {
				t.Fatalf("未打 buildCCRequest 形状日志: %q", out)
			}
			end := i + len(key)
			for end < len(out) && out[end] >= '0' && out[end] <= '9' {
				end++
			}
			got, err := strconv.Atoi(out[i+len(key) : end])
			if err != nil {
				t.Fatalf("orphaned_tool_results 不是数字: %q", out[i+len(key):])
			}
			if got != c.wantOrphan {
				t.Fatalf("orphaned_tool_results = %d, want %d\n  %s", got, c.wantOrphan, out)
			}
			if !strings.Contains(out, "roles=[") || !strings.Contains(out, "fcs=[") {
				t.Errorf("形状日志缺 roles/fcs 字段: %s", out)
			}
			t.Logf("log: %s", out)
		})
	}
}

// TestWriteNonStreamAsSSE_KeepsToolCalls 复现 Kimi Code「无法调用工具」的根因：
//
// 上游非流式响应里 tool_calls 挂在 choices[].message 下，而客户端又要求 stream=true、
// 请求体超过 100KB 走了「非流式→SSE」包装。修复前那段包装只把 message 的 role/content
// 搬进 delta，tool_calls 被整段丢弃，客户端收到 finish_reason="tool_calls" 却没有任何
// 工具调用，没有东西可执行，界面就停死在输入提示符。
// 这条测试断言工具调用必须完整到达客户端，而不是只看 finish_reason。
func TestWriteNonStreamAsSSE_KeepsToolCalls(t *testing.T) {
	upstream := []byte(`{
	  "id": "chatcmpl_test",
	  "object": "chat.completion",
	  "created": 1788936427,
	  "model": "sensenova-6.8-flash-lite",
	  "choices": [{
	    "index": 0,
	    "finish_reason": "tool_calls",
	    "message": {
	      "role": "assistant",
	      "content": "\n\n",
	      "reasoning": "The user wants me to read a file.",
	      "tool_calls": [{
	        "index": 0,
	        "id": "call_a76c86eee4494f7abc2caef7",
	        "type": "function",
	        "function": {"name": "Read", "arguments": "{\"path\": \"F:/src/deepseek_det2/_batch_out.txt\"}"}
	      }]
	    }
	  }],
	  "usage": {"prompt_tokens": 31969, "completion_tokens": 61, "total_tokens": 32030}
	}`)

	rr := httptest.NewRecorder()
	// httptest.ResponseRecorder 本身实现 http.Flusher
	writeNonStreamAsSSE(rr, rr, upstream, "vmodel")
	out := rr.Body.String()
	t.Logf("SSE 输出:\n%s", out)

	lines := strings.Split(out, "\n\n")
	var payload []byte
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "data: ") && !strings.HasPrefix(trimmed, "data: [DONE]") {
			payload = []byte(strings.TrimPrefix(trimmed, "data: "))
		}
	}
	if len(payload) == 0 {
		t.Fatalf("未找到 data 载荷:\n%s", out)
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		t.Fatalf("SSE 载荷不是合法 JSON: %v\n%s", err, payload)
	}
	if chunk["object"] != "chat.completion.chunk" {
		t.Errorf("object = %v, want chat.completion.chunk", chunk["object"])
	}
	choices, _ := chunk["choices"].([]interface{})
	if len(choices) != 1 {
		t.Fatalf("choices 长度 = %d, want 1", len(choices))
	}
	choice, _ := choices[0].(map[string]interface{})
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}
	delta, _ := choice["delta"].(map[string]interface{})
	if delta == nil {
		t.Fatalf("delta 缺失: %s", payload)
	}

	// 核心断言：tool_calls 不能丢。
	tcs, _ := delta["tool_calls"].([]interface{})
	if len(tcs) != 1 {
		t.Fatalf("delta.tool_calls 丢失（这是「无法调用工具」的根因）: %s", payload)
	}
	tc, _ := tcs[0].(map[string]interface{})
	if tc["id"] != "call_a76c86eee4494f7abc2caef7" {
		t.Errorf("tool_calls[0].id = %v", tc["id"])
	}
	// json.Unmarshal 把数字解成 float64，用 JSON 文本比较 index 更稳。
	if b, _ := json.Marshal(tc["index"]); string(b) != "0" {
		t.Errorf("tool_calls[0].index = %s, want 0", b)
	}
	if tc["type"] != "function" {
		t.Errorf("tool_calls[0].type = %v, want function", tc["type"])
	}
	fn, _ := tc["function"].(map[string]interface{})
	if fn == nil {
		t.Fatalf("tool_calls[0].function 缺失: %s", payload)
	}
	if fn["name"] != "Read" {
		t.Errorf("function.name = %v, want Read", fn["name"])
	}
	args, isString := fn["arguments"].(string)
	if !isString {
		t.Errorf("function.arguments 必须是字符串增量: %T %v", fn["arguments"], fn["arguments"])
	} else if args != `{"path": "F:/src/deepseek_det2/_batch_out.txt"}` {
		t.Errorf("function.arguments = %q", args)
	}

	// 反向断言：非标准字段不能被整个 message 灌进 delta，严格客户端会拒收。
	if _, leaked := delta["reasoning"]; leaked {
		t.Errorf("delta 泄漏了非标准字段 reasoning: %s", payload)
	}
	if delta["role"] != "assistant" || delta["content"] != "\n\n" {
		t.Errorf("delta.role/content = %v / %v", delta["role"], delta["content"])
	}
}

// TestWriteNonStreamAsSSE_TextOnlyUnchanged 确保只有文本的普通回复形态不变，
// 并且 content 缺省时补空字符串而不是 null（部分客户端对 null 会解析失败）。
func TestWriteNonStreamAsSSE_TextOnlyUnchanged(t *testing.T) {
	upstream := []byte(`{
	  "id": "chatcmpl_t", "object": "chat.completion", "created": 1,
	  "model": "m",
	  "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "你好"}}]
	}`)
	rr := httptest.NewRecorder()
	writeNonStreamAsSSE(rr, rr, upstream, "m")

	var chunk map[string]interface{}
	payload := []byte{}
	for _, l := range strings.Split(rr.Body.String(), "\n\n") {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "data: ") && !strings.HasPrefix(trimmed, "data: [DONE]") {
			payload = []byte(strings.TrimPrefix(trimmed, "data: "))
		}
	}
	if len(payload) == 0 {
		t.Fatalf("未找到 data 载荷: %s", rr.Body.String())
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		t.Fatalf("载荷不是合法 JSON: %v", err)
	}
	choice := chunk["choices"].([]interface{})[0].(map[string]interface{})
	delta := choice["delta"].(map[string]interface{})
	if delta["content"] != "你好" || delta["role"] != "assistant" {
		t.Errorf("delta = %v", delta)
	}
	if _, has := delta["tool_calls"]; has {
		t.Errorf("纯文本回复不应出现 tool_calls: %s", payload)
	}

	// content 缺失时必须补空串，不能是 null。
	rr2 := httptest.NewRecorder()
	writeNonStreamAsSSE(rr2, rr2, []byte(`{"object":"chat.completion","choices":[{"finish_reason":"stop","message":{"role":"assistant"}}]}`), "m")
	payload2 := []byte{}
	for _, l := range strings.Split(rr2.Body.String(), "\n\n") {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "data: ") && !strings.HasPrefix(trimmed, "data: [DONE]") {
			payload2 = []byte(strings.TrimPrefix(trimmed, "data: "))
		}
	}
	if err := json.Unmarshal(payload2, &chunk); err != nil {
		t.Fatalf("载荷不是合法 JSON: %v", err)
	}
	choice2 := chunk["choices"].([]interface{})[0].(map[string]interface{})
	delta2 := choice2["delta"].(map[string]interface{})
	if c, ok := delta2["content"].(string); !ok || c != "" {
		t.Errorf("content 缺省时应为空字符串，实际 %T %v", delta2["content"], delta2["content"])
	}
}

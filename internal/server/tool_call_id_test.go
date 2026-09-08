package server

import (
	"bytes"
	"encoding/json"
	"log"
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

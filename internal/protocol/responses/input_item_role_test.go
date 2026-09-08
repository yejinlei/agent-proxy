package responses

import (
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// TestItemToMessage_FunctionCallDefaultsAssistantRole 验证 v0.2.120 回归修复：
// Codex 历史里的 function_call item 不带 role 字段，此前转换出的 assistant 工具调用消息
// role 为空 → 上游 400 "field Messages[N].Role invalid, should be set"，整条流 0 delta 失败。
func TestItemToMessage_FunctionCallDefaultsAssistantRole(t *testing.T) {
	item := InputItem{
		Type:      "function_call",
		CallID:    "call_abc123",
		Name:      "exec_command",
		Arguments: `{"cmd":"Get-Content _batch_out.txt"}`,
	}
	msg, ok := itemToMessage(item)
	if !ok {
		t.Fatal("function_call item 必须可转换")
	}
	if msg.Role != schema.Role("assistant") {
		t.Fatalf("缺 role 的 function_call 必须补 role=assistant，实际 %q", msg.Role)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("应产出 1 个工具调用，实际 %d", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_abc123" {
		t.Fatalf("call_id 必须透传，实际 %q", tc.ID)
	}
	if tc.Function.Name != "exec_command" {
		t.Fatalf("工具名必须透传，实际 %q", tc.Function.Name)
	}
	if tc.Function.Arguments != `{"cmd":"Get-Content _batch_out.txt"}` {
		t.Fatalf("参数必须保持 JSON 字符串原样，实际 %q", tc.Function.Arguments)
	}
	t.Logf("✅ function_call → role=assistant, tool=%s args=%s", tc.Function.Name, tc.Function.Arguments)
}

// TestItemToMessage_FunctionCallWithoutCallID 验证 call_id 缺失时合成 ID，不产生空 ID。
func TestItemToMessage_FunctionCallWithoutCallID(t *testing.T) {
	item := InputItem{Type: "function_call", Name: "exec_command", Arguments: "{}"}
	msg, ok := itemToMessage(item)
	if !ok {
		t.Fatal("function_call item 必须可转换")
	}
	if msg.Role != schema.Role("assistant") {
		t.Fatalf("role 应为 assistant，实际 %q", msg.Role)
	}
	if msg.ToolCalls[0].ID == "" {
		t.Fatal("call_id 缺失时必须合成非空 ID")
	}
	t.Logf("✅ 合成 call_id=%q", msg.ToolCalls[0].ID)
}

// TestItemToMessage_MessageWithoutRoleByBlockType 验证省略 role 的 message item
// 按内容块类型推断：input_text 属用户，其余视为 assistant。
func TestItemToMessage_MessageWithoutRoleByBlockType(t *testing.T) {
	cases := []struct {
		blockType string
		want      string
	}{
		{"input_text", "user"},
		{"output_text", "assistant"},
		{"text", "assistant"},
	}
	for _, tc := range cases {
		item := InputItem{
			Type: "message",
			Content: []interface{}{
				map[string]interface{}{"type": tc.blockType, "text": "hello"},
			},
		}
		msg, ok := itemToMessage(item)
		if !ok {
			t.Fatalf("block=%s: item 必须可转换", tc.blockType)
			continue
		}
		if msg.Role != schema.Role(tc.want) {
			t.Fatalf("block=%s: role 应为 %q，实际 %q", tc.blockType, tc.want, msg.Role)
		}
	}
	t.Logf("✅ 省略 role 的 message item 按块类型推断正确")
}

// TestItemToMessage_FunctionCallOutput 验证 function_call_output 转 tool 消息。
func TestItemToMessage_FunctionCallOutput(t *testing.T) {
	item := InputItem{
		Type:   "function_call_output",
		CallID: "call_abc123",
		Output: []interface{}{
			map[string]interface{}{"type": "text", "text": "line1\nline2"},
		},
	}
	msg := functionCallOutputItemToMessage(item)
	if msg.Role != schema.Role("tool") {
		t.Fatalf("function_call_output 必须转 role=tool，实际 %q", msg.Role)
	}
	if msg.ToolCallID != "call_abc123" {
		t.Fatalf("ToolCallID 必须透传，实际 %q", msg.ToolCallID)
	}
	t.Logf("✅ function_call_output → role=tool call_id=%s content=%s", msg.ToolCallID, msg.Content)
}

// TestInputToMessages_MixedHistory 用真实 Codex 会话片段验证端到端：
// 8 条 input（含 function_call + function_call_output）→ 全部落到非空 role 的 message，
// 无任何被静默丢弃的条目。
func TestInputToMessages_MixedHistory(t *testing.T) {
	items := []InputItem{
		{Type: "message", Role: "user", Content: []interface{}{map[string]interface{}{"type": "input_text", "text": "读文件"}}},
		{Type: "message", Role: "assistant", Content: []interface{}{map[string]interface{}{"type": "output_text", "text": "我读了"}}},
		{Type: "function_call", CallID: "call_1", Name: "exec_command", Arguments: `{"cmd":"Get-Content f.txt"}`},
		{Type: "function_call_output", CallID: "call_1", Output: "abc"},
		{Type: "function_call", CallID: "call_2", Name: "apply_patch", Arguments: `{"input":"*** Begin Patch"}`},
		{Type: "function_call_output", CallID: "call_2", Output: []interface{}{map[string]interface{}{"type": "text", "text": "ok"}}},
		{Type: "reasoning"},
		{Type: "message", Role: "user", Content: []interface{}{map[string]interface{}{"type": "input_text", "text": "读取了吗"}}},
	}
	msgs := inputToMessages(items)
	if len(msgs) != 7 {
		t.Fatalf("reasoning 应被丢弃，其余 7 条必须保留，实际 %d", len(msgs))
	}
	for i, m := range msgs {
		if m.Role == "" {
			t.Fatalf("messages[%d] role 不能为空（上游会 400）", i)
		}
	}
	got := ""
	for _, m := range msgs {
		got += string(m.Role) + "/"
	}
	if got != "user/assistant/assistant/tool/assistant/tool/user/" {
		t.Fatalf("role 序列不符预期，实际 %s", got)
	}

	// 工具调用的 ID 与工具结果的 tool_call_id 必须一一配对。
	// 中枢这里保住了配对，若下游 buildCCRequest 丢了 ToolCallID，
	// 上游就会收到悬空工具结果，模型误判文件未读取并停止调工具。
	seen := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			seen[tc.ID] = true
		}
		if m.Role == "tool" && m.ToolCallID == "" {
			t.Fatalf("tool 消息缺 ToolCallID：Codex 的工具结果会变成悬空条目")
		}
		if m.Role == "tool" && !seen[m.ToolCallID] {
			t.Fatalf("tool 消息 %q 在历史里没有对应的 function_call", m.ToolCallID)
		}
	}
	if !seen["call_1"] || !seen["call_2"] {
		t.Fatalf("function_call 的 call_id 未落到 ToolCalls[].ID: %v", seen)
	}
	t.Logf("✅ 混合历史 → %s，工具调用配对 %v", got, seen)
}

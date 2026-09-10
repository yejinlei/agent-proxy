package responses

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// synthName 是发往 CC 上游的合成工具名（exec_<原名>），origName 是 Codex 原 custom 工具名。
// v0.2.133 起映射方向是「合成名 → 原名」：出站必须把 function_call 还原成
// custom_tool_call，且 name 写回 Codex 原名。
var (
	synthName = execPatchToolNameFor([]byte(`{"type":"custom","name":"apply_patch"}`))
	origName  = "apply_patch"
)

func TestExecPatchToolNameFor(t *testing.T) {
	if synthName != "exec_apply_patch" {
		t.Fatalf("execPatchToolNameFor(apply_patch) = %q, want exec_apply_patch", synthName)
	}
	// 特殊字符折叠成 _，保证是合法 CC function 名
	if got := execPatchToolNameFor([]byte(`{"name":"a b/c"}`)); got != "exec_a_b_c" {
		t.Fatalf("execPatchToolNameFor(a b/c) = %q, want exec_a_b_c", got)
	}
	// 空名退化成固定兜底名，工具仍可调用
	if got := execPatchToolNameFor([]byte(`{"type":"custom"}`)); got != "exec_freeform" {
		t.Fatalf("execPatchToolNameFor(no name) = %q, want exec_freeform", got)
	}
}

func TestCollectCustomToolNames(t *testing.T) {
	tools := []schema.InternalTool{
		{Type: "function", Function: &schema.InternalFunction{Name: "read_file"}},
		{Type: "custom", Raw: json.RawMessage(`{"type":"custom","name":"apply_patch"}`), IsCustom: true},
	}
	got := CollectCustomToolNames(tools)
	if got == nil {
		t.Fatalf("CollectCustomToolNames 不应返回 nil")
	}
	if got["exec_apply_patch"] != "apply_patch" {
		t.Fatalf("映射应为 exec_apply_patch→apply_patch，实际 %+v", got)
	}
	if _, ok := got["read_file"]; ok {
		t.Fatalf("普通 function 工具不得进入 custom 映射：%+v", got)
	}
	// 关键回归：IsCustom 工具的 Function 为空时仍然能收集到映射
	if len(CollectCustomToolNames(nil)) != 0 {
		t.Fatalf("空工具表应返回 nil")
	}
	if CollectCustomToolNames([]schema.InternalTool{{Type: "function", Function: &schema.InternalFunction{Name: "x"}}}) != nil {
		t.Fatalf("没有 custom 工具时应返回 nil")
	}
}

// TestTranslateStream_CustomToolCall 验证 custom（freeform）工具的流式出站
// 必须是 type:"custom_tool_call" + input:<裸字符串>，且 name 是 Codex 原名。
//
// Codex 侧证据：protocol/models.rs:1121 ResponseItem::CustomToolCall{call_id,name,input:String}，
// codex-api/src/sse/responses.rs:509-512 对 output_item.done 的 item 直接
// serde_json::from_value::<ResponseItem>。写成 function_call 会让 Codex 把 apply_patch
// 当成没有本地 executor 的未知 function 工具，整轮静默失败。
func TestTranslateStream_CustomToolCall(t *testing.T) {
	tr := NewResponsesTranslator()
	ctx := WithCustomTools(context.Background(), map[string]string{synthName: origName})

	var events []schema.InternalStreamEvent
	events = append(events, schema.InternalStreamEvent{Type: "start", Data: &schema.InternalStreamChunk{Model: "m"}})
	events = append(events, schema.InternalStreamEvent{
		Type: "delta",
		Data: &schema.InternalStreamChunk{Choices: []schema.InternalChoice{{
			Message: schema.InternalMessage{
				ToolCalls: []schema.InternalToolCall{{
					ID: "call_1",
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{Name: synthName, Arguments: `{"input":"*** Begin Patch\n*** Add File: a.txt"}`},
				}},
			},
		}}},
	})
	events = append(events, schema.InternalStreamEvent{
		Type: "done",
		Data: &schema.InternalStreamChunk{Choices: []schema.InternalChoice{{FinishReason: "tool_calls"}}},
	})

	s := string(drainStream(t, tr, ctx, events))
	t.Logf("stream output:\n%s", s)

	if !strings.Contains(s, `"type":"custom_tool_call"`) {
		t.Fatalf("必须输出 type:\"custom_tool_call\"，实际:\n%s", s)
	}
	if strings.Contains(s, `"type":"function_call"`) {
		t.Fatalf("custom 工具禁止输出 type:\"function_call\":\n%s", s)
	}
	if strings.Contains(s, "function_call_arguments") {
		t.Fatalf("custom 工具禁止发 function_call_arguments.* 事件:\n%s", s)
	}
	// 出站 name 必须是 Codex 原名，不能泄漏合成名
	if !strings.Contains(s, `"name":"apply_patch"`) {
		t.Fatalf("出站 name 必须是 Codex 原工具名 apply_patch:\n%s", s)
	}
	if strings.Contains(s, synthName) {
		t.Fatalf("出站禁止出现 CC 合成名 %s:\n%s", synthName, s)
	}
	// input 字段必须是解包后的裸串（Codex 要的就是 freeform 文本本身，不是 JSON）
	if !strings.Contains(s, `"input":"*** Begin Patch\n*** Add File: a.txt"`) {
		t.Fatalf("input 必须是解包后的裸串（去掉 {\"input\":...} 外壳）:\n%s", s)
	}
	if !strings.Contains(s, `"call_id":"call_1"`) {
		t.Fatalf("call_id 必须保留:\n%s", s)
	}
	if !strings.Contains(s, "response.completed") || !strings.Contains(s, `[DONE]`) {
		t.Fatalf("必须发 response.completed + [DONE] 结束序列:\n%s", s)
	}
}

// TestTranslateStream_FunctionCallUnchanged 确认 custom 映射不影响普通 function 工具。
func TestTranslateStream_FunctionCallUnchanged(t *testing.T) {
	tr := NewResponsesTranslator()
	ctx := WithCustomTools(context.Background(), map[string]string{synthName: origName})

	var events []schema.InternalStreamEvent
	events = append(events, schema.InternalStreamEvent{
		Type: "delta",
		Data: &schema.InternalStreamChunk{Choices: []schema.InternalChoice{{
			Message: schema.InternalMessage{
				ToolCalls: []schema.InternalToolCall{{
					ID: "call_2",
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{Name: "read_file", Arguments: `{"path":"a.txt"}`},
				}},
			},
		}}},
	})
	events = append(events, schema.InternalStreamEvent{Type: "done", Data: &schema.InternalStreamChunk{}})

	s := string(drainStream(t, tr, ctx, events))

	if !strings.Contains(s, `"type":"function_call"`) {
		t.Fatalf("普通 function 工具必须仍是 type:\"function_call\":\n%s", s)
	}
	if !strings.Contains(s, `"arguments":{"path":"a.txt"}`) && !strings.Contains(s, `"arguments":"{\"path\":\"a.txt\"}"`) {
		t.Fatalf("arguments 必须是 JSON 字符串:\n%s", s)
	}
	if strings.Contains(s, "custom_tool_call") {
		t.Fatalf("普通 function 工具禁止输出 custom_tool_call:\n%s", s)
	}
}

// TestTranslateStream_EmptyCustomArgs 确认 custom 工具空参数时不兜底 "{}"
// （customFCItem 的约定：custom 没有 JSON 参数对象可兜底，空串如实交付）。
func TestTranslateStream_EmptyCustomArgs(t *testing.T) {
	tr := NewResponsesTranslator()
	ctx := WithCustomTools(context.Background(), map[string]string{synthName: origName})

	events := []schema.InternalStreamEvent{
		{Type: "delta", Data: &schema.InternalStreamChunk{Choices: []schema.InternalChoice{{
			Message: schema.InternalMessage{
				ToolCalls: []schema.InternalToolCall{{
					ID: "call_3",
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{Name: synthName, Arguments: ""},
				}},
			},
		}}}},
		{Type: "done", Data: &schema.InternalStreamChunk{}},
	}

	s := string(drainStream(t, tr, ctx, events))
	if strings.Contains(s, `"arguments":"{}"`) {
		t.Fatalf("custom 工具空参数禁止兜底 \"{}\":\n%s", s)
	}
}

// TestTranslateResponse_CustomToolCall 验证非流式（stream:false）出站同样产出
// custom_tool_call 顶层 item——Codex 只从 output[] 里的独立 item 识别工具调用。
func TestTranslateResponse_CustomToolCall(t *testing.T) {
	tr := NewResponsesTranslator()
	resp := &schema.InternalResponse{
		ID:    "resp_1",
		Model: "m",
		Choices: []schema.InternalChoice{{
			FinishReason: "tool_calls",
			Message: schema.InternalMessage{
				Content: json.RawMessage(`"正在写文件"`),
				ToolCalls: []schema.InternalToolCall{{
					ID: "call_4",
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{Name: synthName, Arguments: `{"input":"*** Begin Patch\n*** End Patch"}`},
				}},
			},
		}},
		Usage:           &schema.InternalUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		CustomToolNames: map[string]string{synthName: origName},
	}

	out, err := tr.TranslateResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	t.Logf("non-stream output:\n%s", s)

	if !strings.Contains(s, `"type":"custom_tool_call"`) {
		t.Fatalf("非流式必须输出 custom_tool_call item:\n%s", s)
	}
	if strings.Contains(s, `"type":"function_call"`) {
		t.Fatalf("custom 工具禁止输出 function_call:\n%s", s)
	}
	if !strings.Contains(s, `"name":"apply_patch"`) {
		t.Fatalf("出站 name 必须是 Codex 原工具名:\n%s", s)
	}
	if strings.Contains(s, synthName) {
		t.Fatalf("出站禁止出现 CC 合成名:\n%s", s)
	}
	if !strings.Contains(s, `"input":"*** Begin Patch\n*** End Patch"`) {
		t.Fatalf("input 必须是解包后的裸串:\n%s", s)
	}
	if !strings.Contains(s, `"output_text"`) {
		t.Fatalf("文本内容块必须保留（与工具调用并存）:\n%s", s)
	}
	if !strings.Contains(s, `"input_tokens":10`) {
		t.Fatalf("usage 必须映射且非空:\n%s", s)
	}
}

// drainStream 喂入事件并收集全部 SSE 字节。
func drainStream(t *testing.T, tr *ResponsesTranslator, ctx context.Context, events []schema.InternalStreamEvent) []byte {
	t.Helper()
	eventsCh := make(chan schema.InternalStreamEvent, len(events))
	for _, e := range events {
		eventsCh <- e
	}
	close(eventsCh)

	var sb strings.Builder
	done := make(chan struct{})
	go func() {
		tr.TranslateStream(ctx, eventsCh, func(data []byte, isDone bool) {
			sb.Write(data)
			if isDone {
				close(done)
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("TranslateStream 未在 10s 内结束")
	}
	return []byte(sb.String())
}

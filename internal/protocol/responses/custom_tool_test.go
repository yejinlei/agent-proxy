package responses

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// TestTranslateStream_CustomToolCall 验证 v0.2.132：custom（freeform）工具的流式出站
// 必须是 type:"custom_tool_call" + input:<裸字符串>，不能是 function_call + arguments。
//
// Codex 侧证据：protocol/models.rs:1121 ResponseItem::CustomToolCall{call_id,name,input:String}，
// codex-api/src/sse/responses.rs:509-512 对 output_item.done 的 item 直接
// serde_json::from_value::<ResponseItem>。写成 function_call 会让 Codex 把 apply_patch
// 当成没有本地 executor 的未知 function 工具，整轮静默失败。
func TestTranslateStream_CustomToolCall(t *testing.T) {
	tr := NewResponsesTranslator()
	ctx := WithCustomTools(context.Background(), map[string]bool{"apply_patch": true})

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
					}{Name: "apply_patch", Arguments: `{"input":"*** Begin Patch\n*** Add File: a.txt"}`},
				}},
			},
		}}},
	})
	events = append(events, schema.InternalStreamEvent{
		Type: "done",
		Data: &schema.InternalStreamChunk{Choices: []schema.InternalChoice{{FinishReason: "tool_calls"}}},
	})

	out := drainStream(t, tr, ctx, events)
	s := string(out)
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

// TestTranslateStream_FunctionCallUnchanged 确认 custom 名表不影响普通 function 工具
// （回归：v0.2.132 只应改变 custom 的出站形状）。
func TestTranslateStream_FunctionCallUnchanged(t *testing.T) {
	tr := NewResponsesTranslator()
	ctx := WithCustomTools(context.Background(), map[string]bool{"apply_patch": true})

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
	ctx := WithCustomTools(context.Background(), map[string]bool{"apply_patch": true})

	events := []schema.InternalStreamEvent{
		{Type: "delta", Data: &schema.InternalStreamChunk{Choices: []schema.InternalChoice{{
			Message: schema.InternalMessage{
				ToolCalls: []schema.InternalToolCall{{
					ID: "call_3",
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{Name: "apply_patch", Arguments: ""},
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
					}{Name: "apply_patch", Arguments: `{"input":"*** Begin Patch\n*** End Patch"}`},
				}},
			},
		}},
		Usage:           &schema.InternalUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		CustomToolNames: map[string]bool{"apply_patch": true},
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

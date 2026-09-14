package responses

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// toolSearchItemEvents 构造一条 function_call 流式事件序列（首片带 id+name，参数分片）。
// 与 custom_tool_test.go 的同型测试一致：TranslateStream 只认 InternalStreamEvent。
func toolSearchItemEvents(id, args string) []schema.InternalStreamEvent {
	return []schema.InternalStreamEvent{
		{Type: "delta", Data: &schema.InternalStreamChunk{Choices: []schema.InternalChoice{{
			Message: schema.InternalMessage{
				ToolCalls: []schema.InternalToolCall{{
					ID: id,
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{Name: ToolSearchFunctionName, Arguments: args},
				}},
			},
		}}}},
		{Type: "done", Data: &schema.InternalStreamChunk{}},
	}
}

// chunkedToolSearchEvents 构造一个分片到达的 tool_call：首片参数是 first，
// 续片追加 second。真实上游（OpenAI 兼容流）几乎总是这样给 tool_call 参数——
// 单片 JSON 只是特例。
func chunkedToolSearchEvents(id, first, second string) []schema.InternalStreamEvent {
	return []schema.InternalStreamEvent{
		{Type: "delta", Data: &schema.InternalStreamChunk{Choices: []schema.InternalChoice{{
			Message: schema.InternalMessage{ToolCalls: []schema.InternalToolCall{{
				ID: id,
				Function: struct {
					Name         string          `json:"name"`
					Arguments    string          `json:"arguments"`
					RawArguments json.RawMessage `json:"-"`
				}{Name: ToolSearchFunctionName, Arguments: first},
			}}},
		}}}},
		{Type: "delta", Data: &schema.InternalStreamChunk{Choices: []schema.InternalChoice{{
			Message: schema.InternalMessage{ToolCalls: []schema.InternalToolCall{{
				Function: struct {
					Name         string          `json:"name"`
					Arguments    string          `json:"arguments"`
					RawArguments json.RawMessage `json:"-"`
				}{Arguments: second},
			}}},
		}}}},
		{Type: "done", Data: &schema.InternalStreamChunk{}},
	}
}

// assertItemTypes 解析流里所有 output_item.added / output_item.done 事件，
// 返回 output_index -> 该 index 出现过的 item type 集合。
func assertItemTypes(t *testing.T, s string) map[int]map[string]int {
	t.Helper()
	out := map[int]map[string]int{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if !strings.Contains(payload, "output_item.added") && !strings.Contains(payload, "output_item.done") {
			continue
		}
		var env struct {
			Type      string          `json:"type"`
			OutputIdx int             `json:"output_index"`
			Item      json.RawMessage `json:"item"`
		}
		if err := json.Unmarshal([]byte(payload), &env); err != nil {
			continue
		}
		var item struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(env.Item, &item); err != nil {
			continue
		}
		if out[env.OutputIdx] == nil {
			out[env.OutputIdx] = map[string]int{}
		}
		out[env.OutputIdx][item.Type]++
		t.Logf("  %s output_index=%d item.type=%q", env.Type, env.OutputIdx, item.Type)
	}
	return out
}

// assertSingleItemTypeDef 断言某个 output_index 的 added/done 只出现一种 item type。
// Codex 的 process_responses_event 把两者独立解析成 ResponseItem：
// OutputItemAdded 走 handle_non_tool_response_item（只建展示用 turn item，
// stream_events_utils.rs:407），OutputItemDone 才走 ToolRouter::build_tool_call
// 派发工具。类型不一致会让 added 注册落空、done 成孤儿 item。
func assertSingleItemTypeDef(t *testing.T, types map[int]map[string]int, idx int) {
	t.Helper()
	got, ok := types[idx]
	if !ok {
		t.Fatalf("output_index=%d 没有任何 added/done 事件", idx)
	}
	if len(got) != 1 {
		t.Fatalf("output_index=%d 的 added/done item type 不一致: %v", idx, got)
	}
}

// TestTranslateStream_ToolSearchArgsAcrossChunks 锁定分片参数的路径。
//
// 真实上游几乎总是把 tool_call 参数拆成多片（首片带 name、参数逐片追加）。
// 单片测试看不出这一层的 bug：SearchValid 若在首片按「当时已有的参数」判定并
// 锁定，分片场景下必然把合法调用误判成非法（首片参数要么为空、要么是 JSON 前缀）。
//
// @AI_GUARD: RESPONSES_TOOL_SEARCH_VALIDITY - 分片参数不得把合法调用降级
// @CONSTRAINT: 参数分散在多个 delta 时，最终 done 必须是 tool_search_call +
//   arguments:<合法对象>，且 added 与 done 的 item type 一致。
// @REASON: v0.2.142 初版在 getFC 里按首片 arguments 锁定 SearchValid，
//   实测「首片只有 name、参数在续片」把合法调用永久降级成 function_call
//   → Codex 以 ToolPayload::Function 派发到 ToolSearchHandler → Fatal。
func TestTranslateStream_ToolSearchArgsAcrossChunks(t *testing.T) {
	tr := NewResponsesTranslator()
	s := string(drainStream(t, tr, context.Background(),
		chunkedToolSearchEvents("call_chunk1", "", `{"query":"codegraph explore","limit":5}`)))
	t.Logf("stream output:\n%s", s)

	if !strings.Contains(s, `"type":"tool_search_call"`) {
		t.Fatalf("分片参数必须最终改写成 tool_search_call:\n%s", s)
	}
	if !strings.Contains(s, `"arguments":{"limit":5,"query":"codegraph explore"}`) {
		t.Fatalf("续片参数必须累积成合法 arguments 对象:\n%s", s)
	}
	if strings.Contains(s, `"arguments":null`) {
		t.Fatalf("arguments 禁止为 null（Codex 侧 query 缺失 → RespondToModel）:\n%s", s)
	}
	// fc 在 output_index=1（0 是 message item），added/done 必须同为 tool_search_call
	types := assertItemTypes(t, s)
	assertSingleItemTypeDef(t, types, 1)
	if _, bad := types[1]["function_call"]; bad {
		t.Fatalf("分片参数合法的 tool_search 禁止落 function_call:\n%s", s)
	}
}

// TestTranslateStream_ToolSearchChunkedPrefix 首片带合法 name、参数是
// JSON **前缀**（单独看必然解析失败），续片补齐。
// 这是最容易让「首片锁定」实现误判的形态：首片 {"query": 单独 Unmarshal 失败。
func TestTranslateStream_ToolSearchChunkedPrefix(t *testing.T) {
	tr := NewResponsesTranslator()
	s := string(drainStream(t, tr, context.Background(),
		chunkedToolSearchEvents("call_chunk2", `{"query":`, `"zvec grep","limit":2}`)))
	t.Logf("stream output:\n%s", s)

	if !strings.Contains(s, `"type":"tool_search_call"`) {
		t.Fatalf("参数前缀分片必须最终改写成 tool_search_call:\n%s", s)
	}
	if !strings.Contains(s, `"query":"zvec grep"`) {
		t.Fatalf("续片参数必须累积完整:\n%s", s)
	}
	if strings.Contains(s, `"arguments":null`) {
		t.Fatalf("done 的 arguments 禁止为 null:\n%s", s)
	}
	assertSingleItemTypeDef(t, assertItemTypes(t, s), 1)
}

// TestTranslateStream_ToolSearchArgsChunkedInvalid 分片后仍然非法的参数
// 必须**一致地**落 function_call（added 与 done 同型），而不是中途切换类型。
func TestTranslateStream_ToolSearchArgsChunkedInvalid(t *testing.T) {
	tr := NewResponsesTranslator()
	s := string(drainStream(t, tr, context.Background(),
		chunkedToolSearchEvents("call_chunk3", `{"q":"`, `wrong key"}`)))
	t.Logf("stream output:\n%s", s)

	if strings.Contains(s, `"type":"tool_search_call"`) {
		t.Fatalf("分片后仍非法的参数禁止改写成 tool_search_call:\n%s", s)
	}
	if !strings.Contains(s, `"type":"function_call"`) {
		t.Fatalf("非法参数必须保留 function_call:\n%s", s)
	}
	assertSingleItemTypeDef(t, assertItemTypes(t, s), 1)
}

// TestTranslateStream_ToolSearchNoNullArguments 锁定 done/completed 的 arguments
// 永远是对象、永不为 null。added 事件不带 arguments 字段（Codex 侧只用于注册，
// handle_non_tool_response_item 对 ToolSearchCall 直接返回 None）。
//
// @AI_GUARD: RESPONSES_TOOL_SEARCH_VALIDITY - done 的 arguments 不得为 null
// @CONSTRAINT: tool_search_call 的 output_item.done / response.completed 里的
//   arguments 必须始终是 JSON 对象。Codex 的 OutputItemDone 分支无条件调
//   ToolRouter::build_tool_call（turn.rs:2597 → stream_events_utils.rs:307），
//   arguments:null 会让 SearchToolCallParams.query 缺失 → RespondToModel，
//   浪费一整轮。
func TestTranslateStream_ToolSearchNoNullArguments(t *testing.T) {
	for _, c := range []struct {
		name   string
		events []schema.InternalStreamEvent
	}{
		{"single_valid", toolSearchItemEvents("call_null1", `{"query":"x"}`)},
		{"chunked_empty_first", chunkedToolSearchEvents("call_null2", "", `{"query":"x"}`)},
		{"chunked_prefix", chunkedToolSearchEvents("call_null3", `{"query":`, `"x"}`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := NewResponsesTranslator()
			s := string(drainStream(t, tr, context.Background(), c.events))
			// 只看 done 与 completed（added 允许不带 arguments）
			for _, marker := range []string{"output_item.done", "response.completed"} {
				for _, line := range strings.Split(s, "\n") {
					if !strings.Contains(line, marker) || !strings.Contains(line, "tool_search_call") {
						continue
					}
					if strings.Contains(line, `"arguments":null`) {
						t.Fatalf("%s 里 tool_search_call 的 arguments 禁止为 null:\n%s", marker, line)
					}
					if strings.Contains(line, `"arguments":{}`) {
						t.Fatalf("%s 里 tool_search_call 的 arguments 禁止是空对象（query 必填）:\n%s", marker, line)
					}
				}
			}
		})
	}
}

// 改写为 type:"tool_search_call" + execution:"client" + arguments:<对象>。
//
// 形状依据 codex-rs/protocol/src/models.rs:1081 ResponseItem::ToolSearchCall：
//   id / call_id / status / execution:String / arguments:serde_json::Value
// 与 function_call 的 arguments:String 相反——arguments 是对象。
//
// 若这里漏改（写成 function_call），Codex router.rs:244 会以
// ToolPayload::Function 派发到 ToolSearchHandler，handlers/tool_search.rs:200-207
// 直接 Fatal "tool_search handler received unsupported payload"——单边合成 = 静默致命。
func TestTranslateStream_ToolSearchCall(t *testing.T) {
	tr := NewResponsesTranslator()
	s := string(drainStream(t, tr, context.Background(),
		toolSearchItemEvents("call_ts1", `{"query":"codegraph explore","limit":5}`)))
	t.Logf("stream output:\n%s", s)

	if !strings.Contains(s, `"type":"tool_search_call"`) {
		t.Fatalf("必须输出 type:\"tool_search_call\":\n%s", s)
	}
	if !strings.Contains(s, `"execution":"client"`) {
		t.Fatalf("必须输出 execution:\"client\":\n%s", s)
	}
	// arguments 必须是对象，不是 function_call 的 JSON 字符串。
	// 注：Go json.Marshal 对 map 按 key 字母序输出，所以断言子串时不能假设 query 在 limit 前。
	if !strings.Contains(s, `"arguments":{"limit":5,"query":"codegraph explore"}`) {
		t.Fatalf("arguments 必须是 JSON 对象（含 query+limit）:\n%s", s)
	}
	if strings.Contains(s, `"arguments":"{`) {
		t.Fatalf("arguments 禁止是字符串（那是 function_call 的形状）:\n%s", s)
	}
	if strings.Contains(s, `"type":"function_call"`) {
		t.Fatalf("tool_search 禁止输出 type:\"function_call\":\n%s", s)
	}
	if strings.Contains(s, "function_call_arguments") {
		t.Fatalf("tool_search_call 禁止发 function_call_arguments.* 事件:\n%s", s)
	}
	if !strings.Contains(s, `"call_id":"call_ts1"`) {
		t.Fatalf("call_id 必须保留:\n%s", s)
	}
	if !strings.Contains(s, "response.completed") || !strings.Contains(s, `[DONE]`) {
		t.Fatalf("必须发 response.completed + [DONE] 结束序列:\n%s", s)
	}
}

// TestTranslateStream_ToolSearchArgsCleaned 验证多余字段被剥离、非正 limit 被丢弃。
// SearchToolCallParams 只有 query + limit，多带字段会让 Codex 的
// serde 反序列化报 "failed to parse tool_search arguments"（多余字段默认拒绝）。
func TestTranslateStream_ToolSearchArgsCleaned(t *testing.T) {
	tr := NewResponsesTranslator()
	s := string(drainStream(t, tr, context.Background(),
		toolSearchItemEvents("call_ts2", `{"query":"search","limit":-3,"bogus":1}`)))

	if !strings.Contains(s, `"arguments":{"query":"search"}`) {
		t.Fatalf("limit 非法时应丢弃该字段，仅保留 query:\n%s", s)
	}
	if strings.Contains(s, `"bogus"`) || strings.Contains(s, `"limit"`) {
		t.Fatalf("多余字段与非正 limit 必须被剥离:\n%s", s)
	}
}

// TestTranslateStream_ToolSearchInvalidArgsKeepsFunctionCall 验证非法 arguments
// 不回退成「一定被 Codex 拒绝」的 tool_search_call，而是保留 function_call + 日志。
//
// 保留 function_call 至少让模型看见自己传错了什么；改成非法 tool_search_call
// 只会拿到 "failed to parse tool_search arguments"，比保留更糟。
func TestTranslateStream_ToolSearchInvalidArgsKeepsFunctionCall(t *testing.T) {
	tr := NewResponsesTranslator()
	s := string(drainStream(t, tr, context.Background(),
		toolSearchItemEvents("call_ts3", `{"q":"wrong key"}`)))

	if strings.Contains(s, `"type":"tool_search_call"`) {
		t.Fatalf("非法 arguments 禁止改写成 tool_search_call:\n%s", s)
	}
	if !strings.Contains(s, `"type":"function_call"`) {
		t.Fatalf("非法 arguments 必须保留 function_call:\n%s", s)
	}
}

// TestTranslateStream_ToolSearchEmptyArgs 验证空 arguments 兜底成 "{}" 的行为
// 不适用于 tool_search——否则会被 IsToolSearchArgsValid 拒绝，退化成 function_call。
// 这里确认不出现 tool_search_call（保留 function_call），也不出现兜底 "{}" 被
// 误当成合法 SearchToolCallParams。
func TestTranslateStream_ToolSearchEmptyArgs(t *testing.T) {
	tr := NewResponsesTranslator()
	s := string(drainStream(t, tr, context.Background(),
		toolSearchItemEvents("call_ts4", "")))

	if strings.Contains(s, `"type":"tool_search_call"`) {
		t.Fatalf("空 arguments 禁止改写成 tool_search_call（query 必填）:\n%s", s)
	}
	if !strings.Contains(s, `"type":"function_call"`) {
		t.Fatalf("空 arguments 必须保留 function_call:\n%s", s)
	}
}

// TestTranslateResponse_ToolSearchCall 验证非流式（stream:false）出站同样产出
// tool_search_call 顶层 item——Codex 只从 output[] 里的独立 item 识别工具调用。
// 与流式路径必须逐字段一致，否则 stream:false 的 Codex 会话拿到 function_call
// → ToolPayload::Function → ToolSearchHandler Fatal。
func TestTranslateResponse_ToolSearchCall(t *testing.T) {
	tr := NewResponsesTranslator()
	resp := &schema.InternalResponse{
		ID:    "resp_ts",
		Model: "m",
		Choices: []schema.InternalChoice{{
			FinishReason: "tool_calls",
			Message: schema.InternalMessage{
				ToolCalls: []schema.InternalToolCall{{
					ID: "call_ts5",
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{Name: ToolSearchFunctionName, Arguments: `{"query":"zvec grep search"}`},
				}},
			},
		}},
		Usage: &schema.InternalUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}

	out, err := tr.TranslateResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	t.Logf("non-stream output:\n%s", s)

	if !strings.Contains(s, `"type":"tool_search_call"`) {
		t.Fatalf("非流式必须输出 tool_search_call item:\n%s", s)
	}
	if !strings.Contains(s, `"execution":"client"`) {
		t.Fatalf("必须输出 execution:\"client\":\n%s", s)
	}
	if !strings.Contains(s, `"arguments":{"query":"zvec grep search"}`) {
		t.Fatalf("arguments 必须是 JSON 对象:\n%s", s)
	}
	if strings.Contains(s, `"type":"function_call"`) {
		t.Fatalf("tool_search 禁止输出 function_call:\n%s", s)
	}
	if !strings.Contains(s, `"call_id":"call_ts5"`) {
		t.Fatalf("call_id 必须保留:\n%s", s)
	}
}

// TestTranslateResponse_ToolSearchInvalidArgsKeepsFunctionCall 验证非流式的
// 非法 arguments 回退行为与流式一致。
func TestTranslateResponse_ToolSearchInvalidArgsKeepsFunctionCall(t *testing.T) {
	tr := NewResponsesTranslator()
	resp := &schema.InternalResponse{
		ID:    "resp_ts_bad",
		Model: "m",
		Choices: []schema.InternalChoice{{
			FinishReason: "tool_calls",
			Message: schema.InternalMessage{
				ToolCalls: []schema.InternalToolCall{{
					ID: "call_ts6",
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{Name: ToolSearchFunctionName, Arguments: `{"query":123}`},
				}},
			},
		}},
		Usage: &schema.InternalUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}

	out, err := tr.TranslateResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, `"type":"tool_search_call"`) {
		t.Fatalf("非法 arguments 禁止改写成 tool_search_call:\n%s", s)
	}
	if !strings.Contains(s, `"type":"function_call"`) {
		t.Fatalf("非法 arguments 必须保留 function_call:\n%s", s)
	}
}

// TestTranslateResponse_ToolSearchNotConfusedWithNamespace 验证一个名字以
// tool_search 开头、但实际是 namespace 展平工具（<ns>_tool_search）的调用
// 不会被误改成 tool_search_call——判定必须精确等于常量，不是前缀匹配。
func TestTranslateResponse_ToolSearchNotConfusedWithNamespace(t *testing.T) {
	tr := NewResponsesTranslator()
	resp := &schema.InternalResponse{
		ID:    "resp_ts_ns",
		Model: "m",
		Choices: []schema.InternalChoice{{
			FinishReason: "tool_calls",
			Message: schema.InternalMessage{
				ToolCalls: []schema.InternalToolCall{{
					ID: "call_ts7",
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{Name: "mcp__codegraph_tool_search", Arguments: `{"query":"x"}`},
				}},
			},
		}},
		Usage: &schema.InternalUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		NamespaceTools: map[string]schema.InternalNamespaceToolRef{
			"mcp__codegraph_tool_search": {Namespace: "mcp__codegraph", ToolName: "tool_search"},
		},
	}

	out, err := tr.TranslateResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, `"type":"tool_search_call"`) {
		t.Fatalf("namespace 展平工具禁止被误改写成 tool_search_call:\n%s", s)
	}
	if !strings.Contains(s, `"type":"function_call"`) || !strings.Contains(s, `"namespace":"mcp__codegraph"`) {
		t.Fatalf("必须是带 namespace 字段的 function_call:\n%s", s)
	}
}

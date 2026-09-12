package responses

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// 本文件覆盖 namespace（Codex MCP / multi_agent）工具的双向桥接。
//
// 核心断言只有一条：**原工具名与命名空间名必须显式携带，禁止从展平名反推**。
// 命名空间名本身可以含下划线（实测 mcp__codegraph / mcp__zvec_grep / multi_agent_v1），
// 按第一个 _ 截前缀会得到 "codegraph" 而不是 "mcp__codegraph"，出站 namespace
// 字段指向不存在的命名空间，整轮工具调用静默失败。

const nsFlatName = "mcp__codegraph_codegraph_explore" // 展平名 = <namespace>_<toolname>

// 展平定义所用的真实 Codex payload 形态：namespace 内嵌 function 子工具。
func nsToolRaw() json.RawMessage {
	return json.RawMessage(`{
		"type":"namespace",
		"name":"mcp__codegraph",
		"description":"Codegraph knowledge graph",
		"tools":[
			{"type":"function","name":"codegraph_explore","description":"查询代码符号",
				"parameters":{"type":"object","properties":{"query":{"type":"string"}}}},
			{"type":"function","name":"codegraph_search"}
		]
	}`)
}

func TestNamespaceToolNameFor(t *testing.T) {
	if got := namespaceToolNameFor("mcp__codegraph", "codegraph_explore"); got != nsFlatName {
		t.Fatalf("展平名派生错误: got %q", got)
	}
	// 空名也不得产生悬空分隔符以外的东西——出站必须按原名还原，不依赖本规则可逆
	if got := namespaceToolNameFor("ns", "x"); got != "ns_x" {
		t.Fatalf("普通命名空间展平名派生错误: got %q", got)
	}
}

func TestNamespaceSubTools(t *testing.T) {
	got := NamespaceSubTools(nsToolRaw())
	if len(got) != 2 {
		t.Fatalf("必须展平出 2 个 function 子工具，实际 %d: %+v", len(got), got)
	}
	if got[0].Tool.Function == nil || got[0].Tool.Function.Name != nsFlatName {
		t.Fatalf("子工具 1 必须展平成 %q，实际 %+v", nsFlatName, got[0].Tool)
	}
	if got[0].OriginalName != "codegraph_explore" {
		t.Fatalf("OriginalName 必须是原工具名 codegraph_explore（禁止按展平名前缀反推），实际 %q", got[0].OriginalName)
	}
	if got[1].Tool.Function.Name != "mcp__codegraph_codegraph_search" {
		t.Fatalf("子工具 2 展平名错误: %+v", got[1].Tool)
	}
	// schema 与 description 必须原样透传，不得丢失或变形
	if p := got[0].Tool.Function.Parameters; p["type"] != "object" {
		t.Fatalf("parameters 未透传: %+v", p)
	}
	if got[0].Tool.Function.Description != "查询代码符号" {
		t.Fatalf("description 未透传: %q", got[0].Tool.Function.Description)
	}
}

// TestNamespaceSubTools_DropsNonFunctionChildren 命名空间内可以嵌 custom freeform 工具
// （Codex 的 ResponsesApiNamespaceTool 是 Function | Custom 二选一），
// 这类子项没有 CC 等价表达，必须丢弃但必须可观测。
func TestNamespaceSubTools_DropsNonFunctionChildren(t *testing.T) {
	raw := json.RawMessage(`{"type":"namespace","name":"multi_agent_v1","tools":[
		{"type":"function","name":"send_message"},
		{"type":"custom","name":"spawn_agent","format":{"type":"grammar","syntax":"freeform"}}
	]}`)
	got := NamespaceSubTools(raw)
	if len(got) != 1 {
		t.Fatalf("只有 1 个 function 子工具可展平（custom 无 CC 等价表达），实际 %d: %+v", len(got), got)
	}
	if got[0].Tool.Function.Name != "multi_agent_v1_send_message" {
		t.Fatalf("展平名错误: %+v", got[0].Tool)
	}
	if got[0].OriginalName != "send_message" {
		t.Fatalf("OriginalName 错误: %q", got[0].OriginalName)
	}
}

func TestNamespaceSubTools_DropsEmptyName(t *testing.T) {
	raw := json.RawMessage(`{"type":"namespace","name":"ns","tools":[
		{"type":"function","name":""},
		{"type":"function","name":"ok_tool"}
	]}`)
	got := NamespaceSubTools(raw)
	if len(got) != 1 {
		t.Fatalf("空名子工具必须丢弃，实际 %d: %+v", len(got), got)
	}
	if got[0].OriginalName != "ok_tool" {
		t.Fatalf("剩余子工具错误: %+v", got[0])
	}
}

func TestNamespaceSubTools_EmptyRaw(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"type":"namespace","name":"ns"}`)} {
		if got := NamespaceSubTools(raw); len(got) != 0 {
			t.Fatalf("空/无子项 namespace 必须返回空表，实际 %+v", got)
		}
	}
}

func TestCollectNamespaceToolNames(t *testing.T) {
	tools := []schema.InternalTool{
		{Type: "function", Function: &schema.InternalFunction{Name: "exec_command"}},
		{Type: "namespace", Raw: nsToolRaw()},
	}
	got := CollectNamespaceToolNames(tools)
	if len(got) != 2 {
		t.Fatalf("必须收集 2 个展平映射，实际 %+v", got)
	}
	want := NamespaceEntry{Namespace: "mcp__codegraph", ToolName: "codegraph_explore"}
	if got[nsFlatName] != want {
		t.Fatalf("映射必须精确到 <原命名空间名, 原工具名>（下划线命名空间不得失真），实际 %+v，期望 %+v", got[nsFlatName], want)
	}
	if got["mcp__codegraph_codegraph_search"].ToolName != "codegraph_search" {
		t.Fatalf("第二个子工具映射错误: %+v", got)
	}
	// 普通 function 工具不得进入 namespace 映射
	if _, ok := got["exec_command"]; ok {
		t.Fatalf("普通 function 工具不得进入映射: %+v", got)
	}
}

func TestCollectNamespaceToolNames_NilWithoutNamespace(t *testing.T) {
	tools := []schema.InternalTool{
		{Type: "function", Function: &schema.InternalFunction{Name: "a"}},
		{Type: "custom", Raw: json.RawMessage(`{"type":"custom","name":"apply_patch"}`), IsCustom: true},
		{Type: "web_search", Raw: json.RawMessage(`{"type":"web_search"}`)},
	}
	if got := CollectNamespaceToolNames(tools); got != nil {
		t.Fatalf("没有 namespace 工具时必须返回 nil（调用方靠 nil 走原路径），实际 %+v", got)
	}
	if got := CollectNamespaceToolNames(nil); got != nil {
		t.Fatalf("空工具表必须返回 nil，实际 %+v", got)
	}
}

// TestCollectNamespaceToolNames_KeepsFirstOnConflict 两个命名空间展平出同名工具时
// 必须保留第一个并打日志——后到的被覆盖会让出站映射不唯一。
func TestCollectNamespaceToolNames_KeepsFirstOnConflict(t *testing.T) {
	a := json.RawMessage(`{"type":"namespace","name":"ns_a","tools":[{"type":"function","name":"tool","description":"first"}]}`)
	b := json.RawMessage(`{"type":"namespace","name":"ns_b","tools":[{"type":"function","name":"tool","description":"second"}]}`)
	got := CollectNamespaceToolNames([]schema.InternalTool{
		{Type: "namespace", Raw: a},
		{Type: "namespace", Raw: b},
	})
	if got == nil || got["ns_a_tool"] == (NamespaceEntry{}) {
		t.Fatalf("冲突时必须保留第一个映射，实际 %+v", got)
	}
	if got["ns_a_tool"] != (NamespaceEntry{Namespace: "ns_a", ToolName: "tool"}) {
		t.Fatalf("冲突时必须保留第一个（ns_a），实际 %+v", got["ns_a_tool"])
	}
}

// TestNamespaceToolNames_AroundTrip 入站（Codex → CC）与出站（CC → Codex）
// 必须是同一个映射：入站把 namespace 调用还原成展平名，出站再用同一张表还原回原名。
// 这里直接喂一份真实的 Codex 入站 payload，验证展平名与映射表 key 一致。
func TestNamespaceToolNames_AroundTrip(t *testing.T) {
	tr := NewResponsesTranslator()
	raw := json.RawMessage(`{"model":"gpt-5.2","stream":true,"tools":[` + string(nsToolRaw()) + `],
		"input":[
			{"type":"message","role":"user","content":"查一下这个函数"},
			{"type":"function_call","call_id":"call_1","namespace":"mcp__codegraph",
				"name":"codegraph_explore","arguments":"{\"query\":\"AuthService\"}"}
		]}`)
	ir, err := tr.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	// 入站工具定义展平出的映射，key 必须等于历史里那条调用的展平名
	names := CollectNamespaceToolNames(ir.Tools)
	if len(names) != 2 {
		t.Fatalf("入站 tools 必须收集到 2 个展平映射，实际 %+v", names)
	}
	// 历史里的 namespace 调用必须被改写成展平名 + Namespace 字段
	var tc *schema.InternalToolCall
	var toolMsg *schema.InternalMessage
	for i, m := range ir.Messages {
		if len(m.ToolCalls) > 0 {
			tc = &m.ToolCalls[0]
			toolMsg = &ir.Messages[i]
		}
	}
	if tc == nil {
		t.Fatalf("namespace function_call item 必须落成 tool call，messages=%+v", ir.Messages)
	}
	if tc.Function.Name != nsFlatName {
		t.Fatalf("入站 namespace 调用必须还原成展平名 %q，实际 %q", nsFlatName, tc.Function.Name)
	}
	if tc.Namespace != "mcp__codegraph" {
		t.Fatalf("Namespace 字段必须携带命名空间名，实际 %q", tc.Namespace)
	}
	if tc.ID != "call_1" {
		t.Fatalf("call_id 必须保留（工具调用与结果按 call_id 配对），实际 %q", tc.ID)
	}
	if tc.Function.Arguments != `{"query":"AuthService"}` {
		t.Fatalf("arguments 必须原样透传，实际 %q", tc.Function.Arguments)
	}
	// 关键闭环：出站按展平名索引这张表，必须查得到
	if names[tc.Function.Name] != (NamespaceEntry{Namespace: "mcp__codegraph", ToolName: "codegraph_explore"}) {
		t.Fatalf("展平名在映射表里查不到 → 出站无法还原 namespace 字段: tc=%q table=%+v", tc.Function.Name, names)
	}
	if toolMsg.Role != "assistant" {
		t.Fatalf("工具调用必须挂在 assistant 上，实际 %s", toolMsg.Role)
	}
}

// TestInboundNamespaceToolResult 工具结果同样带 namespace 字段，
// 必须落到 role:tool 消息上，否则下一轮出站无法还原。
func TestInboundNamespaceToolResult(t *testing.T) {
	tr := NewResponsesTranslator()
	raw := json.RawMessage(`{"model":"m","input":[
		{"type":"message","role":"user","content":"查一下"},
		{"type":"function_call","call_id":"call_2","namespace":"mcp__codegraph","name":"codegraph_explore","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_2","namespace":"mcp__codegraph","output":"found"}
	]}`)
	ir, err := tr.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	var toolMsg *schema.InternalMessage
	for i, m := range ir.Messages {
		if m.Role == "tool" {
			toolMsg = &ir.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatalf("function_call_output 必须落成 role:tool 消息，messages=%+v", ir.Messages)
	}
	if toolMsg.ToolCallID != "call_2" {
		t.Fatalf("ToolCallID 必须保留（配对靠它），实际 %q", toolMsg.ToolCallID)
	}
	if toolMsg.ToolCallNamespace != "mcp__codegraph" {
		t.Fatalf("ToolCallNamespace 必须携带命名空间名，实际 %q", toolMsg.ToolCallNamespace)
	}
	var got string
	if err := json.Unmarshal(toolMsg.Content, &got); err != nil || got != "found" {
		t.Fatalf("工具输出必须保留，实际 %q", got)
	}
}

// TestTranslateResponse_NamespaceFunctionCall 非流式出站：展平名必须还原成
// name:<原工具名> + namespace:<命名空间名>，type 仍是 function_call。
func TestTranslateResponse_NamespaceFunctionCall(t *testing.T) {
	tr := NewResponsesTranslator()
	resp := &schema.InternalResponse{
		ID:    "resp_1",
		Model: "m",
		Choices: []schema.InternalChoice{{
			FinishReason: "tool_calls",
			Message: schema.InternalMessage{
				Content: json.RawMessage(`"先查一下"`),
				ToolCalls: []schema.InternalToolCall{{
					ID: "call_1",
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{Name: nsFlatName, Arguments: `{"query":"AuthService"}`},
				}},
			},
		}},
		Usage: &schema.InternalUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		NamespaceTools: map[string]NamespaceEntry{
			nsFlatName: {Namespace: "mcp__codegraph", ToolName: "codegraph_explore"},
		},
	}
	out, err := tr.TranslateResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	t.Logf("non-stream output:\n%s", s)

	if !strings.Contains(s, `"type":"function_call"`) {
		t.Fatalf("namespace 工具出站 type 必须是 function_call:\n%s", s)
	}
	if !strings.Contains(s, `"namespace":"mcp__codegraph"`) {
		t.Fatalf("必须写出 namespace 字段（漏掉时 Codex 当成顶层函数执行 → 静默失败）:\n%s", s)
	}
	if !strings.Contains(s, `"name":"codegraph_explore"`) {
		t.Fatalf("name 必须还原成 Codex 原工具名:\n%s", s)
	}
	if strings.Contains(s, nsFlatName) {
		t.Fatalf("禁止泄漏 CC 展平名 %q:\n%s", nsFlatName, s)
	}
	if !strings.Contains(s, `"call_id":"call_1"`) {
		t.Fatalf("call_id 必须保留:\n%s", s)
	}
	if !strings.Contains(s, `"arguments":"{\"query\":\"AuthService\"}"`) {
		t.Fatalf("arguments 必须是 JSON 字符串且原样透传:\n%s", s)
	}
}

// TestTranslateResponse_PlainFunctionCallNoNamespace 确认映射只作用于 namespace 工具，
// 普通 function 工具不得被加上 namespace 字段。
func TestTranslateResponse_PlainFunctionCallNoNamespace(t *testing.T) {
	tr := NewResponsesTranslator()
	resp := &schema.InternalResponse{
		ID:    "resp_2",
		Model: "m",
		Choices: []schema.InternalChoice{{
			FinishReason: "tool_calls",
			Message: schema.InternalMessage{
				ToolCalls: []schema.InternalToolCall{{
					ID: "call_9",
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{Name: "exec_command", Arguments: `{"cmd":"ls"}`},
				}},
			},
		}},
		Usage:          &schema.InternalUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		NamespaceTools: map[string]NamespaceEntry{nsFlatName: {Namespace: "mcp__codegraph", ToolName: "codegraph_explore"}},
	}
	out, err := tr.TranslateResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "namespace") {
		t.Fatalf("普通 function 工具禁止出现 namespace 字段:\n%s", s)
	}
	if !strings.Contains(s, `"name":"exec_command"`) {
		t.Fatalf("普通 function 工具名不得被改写:\n%s", s)
	}
}

// TestTranslateStream_NamespaceFunctionCall 流式出站与非流式必须产出同一形状：
// function_call + namespace 字段 + 原工具名。
func TestTranslateStream_NamespaceFunctionCall(t *testing.T) {
	tr := NewResponsesTranslator()
	ctx := WithNamespaceTools(context.Background(), map[string]NamespaceEntry{
		nsFlatName: {Namespace: "mcp__codegraph", ToolName: "codegraph_explore"},
	})

	events := []schema.InternalStreamEvent{
		{Type: "start", Data: &schema.InternalStreamChunk{Model: "m"}},
		{Type: "delta", Data: &schema.InternalStreamChunk{Choices: []schema.InternalChoice{{
			Message: schema.InternalMessage{
				ToolCalls: []schema.InternalToolCall{{
					ID: "call_1",
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{Name: nsFlatName, Arguments: `{"query":"AuthService"}`},
				}},
			},
		}}}},
		{Type: "done", Data: &schema.InternalStreamChunk{Choices: []schema.InternalChoice{{FinishReason: "tool_calls"}}}},
	}

	s := string(drainStream(t, tr, ctx, events))
	t.Logf("stream output:\n%s", s)

	if !strings.Contains(s, `"type":"function_call"`) {
		t.Fatalf("必须输出 type:\"function_call\":\n%s", s)
	}
	if !strings.Contains(s, `"namespace":"mcp__codegraph"`) {
		t.Fatalf("流式出口必须写 namespace 字段（与非流式保持一致）:\n%s", s)
	}
	if !strings.Contains(s, `"name":"codegraph_explore"`) {
		t.Fatalf("流式出口 name 必须还原成原工具名:\n%s", s)
	}
	if strings.Contains(s, `"name":"`+nsFlatName+`"`) {
		t.Fatalf("整个流式输出禁止出现 CC 展平名——item 级（output_item.added/done、response.completed.output[]）与"+
			"事件级（function_call_arguments.done 的裸 name）都必须还原成原工具名:\n%s", s)
	}
	if strings.Contains(s, "custom_tool_call") {
		t.Fatalf("namespace 工具不是 custom 工具，禁止输出 custom_tool_call:\n%s", s)
	}
	if !strings.Contains(s, "response.completed") || !strings.Contains(s, `[DONE]`) {
		t.Fatalf("必须发 response.completed + [DONE] 结束序列:\n%s", s)
	}

	// v0.2.138 起 END 汇总行必须同时带 ns_tools / ns_calls，否则"工具送达了但模型没用"
	// 只能靠人工数 fcs=[...] 里的工具名——与 v0.2.132 之前"连丢了哪些工具都看不到"是同类盲区。
	// ns_tools 来自 ctx 映射表（本轮送出去的展平工具数），ns_calls 来自模型实际发出的调用。
	gotLog := nsLogCapture(t, tr, ctx, events)
	if !strings.Contains(gotLog, "TranslateStream END") {
		t.Fatalf("未捕获 END 汇总日志：%q", gotLog)
	}
	for _, want := range []string{"ns_tools=1", "ns_calls=1"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("END 汇总日志必须带 %s（否则送达侧与使用侧不可 grep）：%q", want, gotLog)
		}
	}
}

// nsLogCapture 把 TranslateStream 运行期间的 log 输出捕获成字符串，仅用于断言汇总日志字段。
// 命名前缀 ns 避免与本包内其他 translate stream 日志捕获 helper 撞名。
func nsLogCapture(t *testing.T, tr *ResponsesTranslator, ctx context.Context, events []schema.InternalStreamEvent) string {
	t.Helper()
	captured := newSyncBuffer()
	old := log.Writer()
	log.SetOutput(captured)
	defer log.SetOutput(old)

	_ = drainStream(t, tr, ctx, events)
	captured.Close()
	return captured.String()
}

// TestTranslateStream_PlainFunctionCallWithoutNamespaceContext 映射表为空时
// 行为必须与改动前完全一致——这是防止"修 A 坏 B"的护栏。
func TestTranslateStream_PlainFunctionCallWithoutNamespaceContext(t *testing.T) {
	tr := NewResponsesTranslator()
	events := []schema.InternalStreamEvent{
		{Type: "delta", Data: &schema.InternalStreamChunk{Choices: []schema.InternalChoice{{
			Message: schema.InternalMessage{
				ToolCalls: []schema.InternalToolCall{{
					ID: "call_5",
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{Name: "exec_command", Arguments: `{"cmd":"ls"}`},
				}},
			},
		}}}},
		{Type: "done", Data: &schema.InternalStreamChunk{}},
	}
	s := string(drainStream(t, tr, context.Background(), events))
	if strings.Contains(s, "namespace") {
		t.Fatalf("无 namespace 映射时禁止出现 namespace 字段:\n%s", s)
	}
	if !strings.Contains(s, `"type":"function_call"`) || !strings.Contains(s, `"name":"exec_command"`) {
		t.Fatalf("普通 function 工具形状必须不变:\n%s", s)
	}

	// 零值也必须可观测：ns_tools=0 说明本轮上游没有 namespace 工具，
	// 与 ns_tools>0 && ns_calls=0（"送达了但没用"）是两种不同状态，日志上必须能区分。
	gotLog := nsLogCapture(t, tr, context.Background(), events)
	for _, want := range []string{"ns_tools=0", "ns_calls=0"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("无 namespace 映射时 END 日志必须为 %s：%q", want, gotLog)
		}
	}
}

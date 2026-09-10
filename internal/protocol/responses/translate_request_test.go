package responses

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// TestTranslateRequest_KeepsNonFunctionTools 验证 v0.2.132：
// Codex 经 WS 发来的 tools 含客户端内置工具（tool_search/web_search/custom 等）。
// TranslateRequest 必须**原样保留全部工具**（禁止按 type 白名单丢弃），因为：
//  1. type:"custom" 的 apply_patch 是 Codex 唯一能改文件的 freeform 工具，
//     v0.2.131 之前被白名单整条丢弃，模型被要求用它改文件却拿不到工具；
//  2. ResponseRequest.Tools 用 []json.RawMessage 承载，非 function 工具出站可原样回写，
//     不能因为 CC 上游不认就把它们从 Central Schema 里删掉（下游可能是原生 Responses 上游）。
//
// 真正需要挡的只有「function 工具但 name 为空」——那会让上游报 400 Invalid request format。
// 能力上限（CC 没有等价表达）由 buildCCRequest 按 Function==nil 丢弃并打日志，
// 而不是在入站翻译时静默删除。
func TestTranslateRequest_KeepsNonFunctionTools(t *testing.T) {
	tr := NewResponsesTranslator()
	// 模拟 Codex 真实 payload：末尾两个是客户端执行的内置工具，中间是 freeform 工具
	raw := json.RawMessage(`{
		"model":"gpt-5.2",
		"input":[{"type":"message","role":"user","content":"hi"}],
		"stream":true,
		"tools":[
			{"type":"function","name":"read_file","parameters":{"type":"object"}},
			{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"rl-diff"}},
			{"type":"tool_search","execution":"client"},
			{"type":"web_search","external_web_access":false}
		]
	}`)
	ir, err := tr.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ir.Tools); got != 4 {
		t.Fatalf("必须保留全部 4 个工具（禁止 type 白名单丢弃），实际 %d", got)
	}
	for _, tl := range ir.Tools {
		if tl.Function != nil && tl.Function.Name == "" {
			t.Fatalf("不应存在 function.name 为空的 tool: %+v", tl)
		}
	}
	// @AI_GUARD 回归：custom 工具的 Function 必须为空。
	// v0.2.132 在这里给 IsCustom 工具填了 Function={input:string} + 合成描述，
	// 而 buildCCRequest 对任何 Function!=nil 的工具都会当普通 CC function 原样转发——
	// 结果 (a) custom 工具以 exec_* 形状被 CC 上游当成一个真实函数，
	// (b) 合成描述串到 7 个普通 function 工具上，把它们的 required 数组盖掉。
	// 合成只能发生在 buildCCRequest 一处。
	var customOK bool
	for _, tl := range ir.Tools {
		if tl.IsCustom {
			customOK = true
			if tl.Function != nil {
				t.Fatalf("custom 工具的 Function 必须为空（合成只能发生在 buildCCRequest），实际 %+v", tl)
			}
			if tl.Raw == nil {
				t.Fatalf("custom 工具必须保留 Raw 原始字节（合成名派生 + 原生 Responses 上游原样回写都靠它）")
			}
			if got := toolNameFromRaw(tl.Raw); got != "apply_patch" {
				t.Fatalf("Raw 里必须能取回原工具名 apply_patch，实际 %q", got)
			}
		}
	}
	if !customOK {
		t.Fatalf("type:\"custom\" 的 apply_patch 必须保留且标记 IsCustom，实际 %+v", ir.Tools)
	}
	// 普通 function 工具保持自己的描述与 schema，不得被 freeform 合成描述污染。
	var readOK bool
	for _, tl := range ir.Tools {
		if tl.Function != nil && tl.Function.Name == "read_file" {
			readOK = true
			if strings.Contains(tl.Function.Description, "input") && strings.Contains(tl.Function.Description, "raw text") {
				t.Fatalf("普通 function 工具的描述不得带 freeform 裸文本说明：%q", tl.Function.Description)
			}
		}
	}
	if !readOK {
		t.Fatalf("普通 function 工具 read_file 必须带 Function，实际 %+v", ir.Tools)
	}
	t.Logf("✅ 全部 %d 个工具已保留（含 custom + 客户端内置工具）", len(ir.Tools))
}

// TestTranslateRequest_EmptyInputInjectsUser 验证 v0.2.109 Fix B：
// Codex 连接预热发 input:[] 请求，inputToMessages 转换后无消息，
// TranslateRequest 必须注入一条 user 消息兜底，
// 否则上游只有 system 无 user 会报 400 "No user query found in messages."
func TestTranslateRequest_EmptyInputInjectsUser(t *testing.T) {
	tr := NewResponsesTranslator()
	raw := json.RawMessage(`{
		"model":"gpt-5.2",
		"input":[],
		"instructions":"you are a helpful assistant",
		"stream":false
	}`)
	ir, err := tr.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(ir.Messages) == 0 {
		t.Fatalf("空 input 必须注入 user 消息兜底，实际 messages=0")
	}
	// 注入的必须是 user 角色
	var hasUser bool
	for _, m := range ir.Messages {
		if m.Role == schema.RoleUser {
			hasUser = true
		}
	}
	if !hasUser {
		t.Fatalf("注入的消息必须含 user 角色，实际 %+v", ir.Messages)
	}
	t.Logf("✅ 空 input 已注入 user 消息，messages=%d", len(ir.Messages))
}

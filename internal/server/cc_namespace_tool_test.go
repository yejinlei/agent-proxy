package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/responses"
	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// TestBuildCCRequest_NamespaceToolFlatten 验证 v0.2.137 的 namespace 工具展平边界。
//
// v0.2.136 之前 namespace 工具落入 buildCCRequest 的 nUnsupported 分支，
// 每次请求固定丢掉 mcp__codegraph / mcp__zvec_grep / multi_agent_v1（tools=14→10）。
// Codex 系统提示明确要求用这些 MCP 工具做证据检索，模型拿不到只能退化到 exec_command。
//
// 展平不是"提升成顶层 function 就完事"：出站 function_call item 必须写
// name:<原工具名> + namespace:<命名空间名>（type 仍是 function_call），
// 而还原依赖 CollectNamespaceToolNames 按展平名索引的映射。所以这里同时验证
// 两个事实：发上游的工具名，与映射表里的 key，必须是同一个字符串。
func TestBuildCCRequest_NamespaceToolFlatten(t *testing.T) {
	const (
		nsRaw = `{"type":"namespace","name":"mcp__codegraph","description":"Codegraph knowledge graph","tools":[
			{"type":"function","name":"codegraph_explore","description":"查询代码符号",
				"parameters":{"type":"object","properties":{"query":{"type":"string"}}}},
			{"type":"function","name":"codegraph_search"}
		]}`
		plainDesc = "Read a file from the workspace."
	)

	ir := &schema.InternalRequest{
		Model:    "sensenova-6.8-flash-lite",
		Messages: []schema.InternalMessage{{Role: "user", Content: json.RawMessage(`"查一下这个函数"`)}},
		Tools: []schema.InternalTool{
			{
				Type: "function",
				Function: &schema.InternalFunction{
					Name:        "exec_command",
					Description: plainDesc,
					Parameters:  map[string]interface{}{"type": "object"},
				},
			},
			// namespace 工具：Function 为空、类型不是 function → 只可能走展平分支。
			{Type: "namespace", Raw: json.RawMessage(nsRaw)},
			// 客户端内置工具：CC 无法表达，必须丢弃且不得污染别的工具。
			{Type: "web_search", Raw: json.RawMessage(`{"type":"web_search","external_web_access":false}`)},
		},
	}

	req := buildCCRequest(ir, "https://api.sensenova.com/v1")

	// 1 个原始 function + namespace 里的 2 个 function 子工具；web_search 必须被丢弃。
	if len(req.Tools) != 3 {
		t.Fatalf("必须发出 3 个工具（1 个原始 function + 2 个展平子工具），实际 %d: %+v", len(req.Tools), req.Tools)
	}

	got := map[string]json.RawMessage{}
	for _, tool := range req.Tools {
		if tool.Function == nil {
			t.Fatalf("每个工具都必须有 Function（CC 上游只认 type:function）: %+v", tool)
		}
		if tool.Type != "function" {
			t.Fatalf("展平后工具类型必须是 function，实际 %q: %+v", tool.Type, tool)
		}
		raw, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		got[tool.Function.Name] = raw
	}

	// 2. 两个子工具都必须以 <namespace>_<toolname> 形状出现。
	for _, want := range []string{"mcp__codegraph_codegraph_explore", "mcp__codegraph_codegraph_search"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("展平工具 %q 必须发上游，实际工具名: %v", want, names(got))
		}
	}
	// 原命名空间名不能作为工具名泄漏（那是 namespace 的 name 字段，不是工具名）。
	if _, ok := got["mcp__codegraph"]; ok {
		t.Fatalf("命名空间名不得被当成工具名发上游: %v", names(got))
	}

	// 3. schema 与 description 必须原样透传——展平只改名字，不能顺带丢参数。
	explore := string(got["mcp__codegraph_codegraph_explore"])
	if !strings.Contains(explore, `"description":"查询代码符号"`) {
		t.Fatalf("子工具 description 未透传:\n%s", explore)
	}
	if !strings.Contains(explore, `"query"`) || !strings.Contains(explore, `"type":"object"`) {
		t.Fatalf("子工具 parameters 未透传:\n%s", explore)
	}
	// 无 parameters 的子工具也要拿到合法的空 schema，否则上游 400。
	search := string(got["mcp__codegraph_codegraph_search"])
	if !strings.Contains(search, `"type":"object"`) {
		t.Fatalf("无 parameters 的子工具必须兜底成空 object schema:\n%s", search)
	}

	// 4. 普通 function 工具保持自己的描述，不得被展平过程污染。
	plain, ok := got["exec_command"]
	if !ok {
		t.Fatalf("原始 function 工具必须保留: %v", names(got))
	}
	if !strings.Contains(string(plain), plainDesc) {
		t.Fatalf("原始 function 描述被污染:\n%s", string(plain))
	}

	// 5. 关键闭环：出站还原按展平名索引这张表，key 必须与上面发上游的工具名一致。
	names := responses.CollectNamespaceToolNames(ir.Tools)
	if len(names) != 2 {
		t.Fatalf("必须收集 2 个展平映射（非流式出站靠它还原 namespace 字段），实际 %+v", names)
	}
	wantEntry := responses.NamespaceEntry{Namespace: "mcp__codegraph", ToolName: "codegraph_explore"}
	if names["mcp__codegraph_codegraph_explore"] != wantEntry {
		t.Fatalf("映射必须精确到 <原命名空间名, 原工具名>（下划线命名空间不得失真），实际 %+v", names["mcp__codegraph_codegraph_explore"])
	}
	if names["mcp__codegraph_codegraph_search"].ToolName != "codegraph_search" {
		t.Fatalf("第二个子工具映射错误: %+v", names)
	}
	// 映射表必须不含普通 function 工具名，否则出站会把它们写成带 namespace 的调用。
	if _, ok := names["exec_command"]; ok {
		t.Fatalf("普通 function 工具不得进入 namespace 映射: %+v", names)
	}

	// 6. 映射表为空时必须返回 nil——调用方靠 nil 走原路径，空 map 与 nil 语义不同。
	if got := responses.CollectNamespaceToolNames(ir.Tools[:1]); got != nil {
		t.Fatalf("没有 namespace 工具时必须返回 nil，实际 %+v", got)
	}
}

func names(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

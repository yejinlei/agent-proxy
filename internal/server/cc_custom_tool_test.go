package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// TestBuildCCRequest_CustomToolSynthesis 验证 v0.2.133 的 custom 工具合成边界。
//
// v0.2.132 的回归：合成放在入站翻译器里（给 IsCustom 工具填 Function shim），
// 而 buildCCRequest 对任何 Function!=nil 的工具都当普通 CC function 原样转发，
// 结果是 (a) 合成描述后缀串到了 7 个普通 function 工具上，把它们的 parameters
// 和 required 数组盖掉；(b) 合成工具名 exec_patch_tool 被当成真实函数发上游，
// custom_tool_call 出站计数为 0（agent-proxy-9093.log）。
//
// 现在合成只能发生在 buildCCRequest 这一处：IsCustom 工具在中央模型里 Function 为空，
// 只有走到 CC 上游边界时才被合成成 exec_<原名>，其它协议出口按 Raw 原样透传。
func TestBuildCCRequest_CustomToolSynthesis(t *testing.T) {
	const (
		customRaw = `{"type":"custom","name":"apply_patch","description":"Apply a patch to the codebase.","format":{"type":"grammar","syntax":"rl-diff"}}`
		plainDesc = "Read a file from the workspace."
	)

	ir := &schema.InternalRequest{
		Model:    "sensenova-6.8-flash-lite",
		Messages: []schema.InternalMessage{{Role: "user", Content: json.RawMessage(`"write a file"`)}},
		Tools: []schema.InternalTool{
			{
				Type: "function",
				Function: &schema.InternalFunction{
					Name:        "exec_command",
					Description: plainDesc,
					Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{"cmd": map[string]interface{}{"type": "string"}}, "required": []string{"cmd"}},
				},
			},
			// Function 必须为空——这是 v0.2.133 的不变量。
			{Type: "custom", Raw: json.RawMessage(customRaw), IsCustom: true},
			// 客户端内置工具：CC 无法表达，必须被丢弃但不得污染别的工具。
			{Type: "web_search", Raw: json.RawMessage(`{"type":"web_search","external_web_access":false}`)},
		},
	}

	req := buildCCRequest(ir, "https://api.sensenova.com/v1")

	if len(req.Tools) != 2 {
		t.Fatalf("必须发出 2 个工具（1 个原始 function + 1 个 custom 合成），实际 %d: %+v", len(req.Tools), req.Tools)
	}

	byName := map[string]json.RawMessage{}
	for _, tool := range req.Tools {
		raw, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		byName[tool.Function.Name] = raw
	}

	// 1. custom 工具必须合成成 exec_apply_patch，且参数是 {input:<string>}。
	synth, ok := byName["exec_apply_patch"]
	if !ok {
		t.Fatalf("custom 工具必须合成成 exec_apply_patch，实际工具名: %+v", byName)
	}
	synthS := string(synth)
	if !strings.Contains(synthS, `"type":"function"`) {
		t.Fatalf("合成工具必须是 type:\"function\"（CC 上游只认 function）:\n%s", synthS)
	}
	if !strings.Contains(synthS, `"required":["input"]`) {
		t.Fatalf("合成工具参数必须 required input:\n%s", synthS)
	}
	// 2. 合成的描述后缀只能出现在 custom 工具上。
	if !strings.Contains(synthS, "raw text payload") {
		t.Fatalf("合成工具描述必须带 freeform 裸文本说明，否则模型会返回空参数:\n%s", synthS)
	}

	// 3. 普通 function 工具保持自己的描述、parameters、required——v0.2.132 这里被污染。
	plain, ok := byName["exec_command"]
	if !ok {
		t.Fatalf("普通 function 工具 exec_command 必须原样转发，实际工具名: %+v", byName)
	}
	plainS := string(plain)
	if !strings.Contains(plainS, plainDesc) {
		t.Fatalf("普通 function 工具的描述必须保持原文:\n%s", plainS)
	}
	if strings.Contains(plainS, "raw text payload") {
		t.Fatalf("普通 function 工具的描述禁止被 freeform 合成后缀污染:\n%s", plainS)
	}
	if !strings.Contains(plainS, `"required":["cmd"]`) {
		t.Fatalf("普通 function 工具的 required 数组必须保持原样（v0.2.132 被合成 schema 盖掉）:\n%s", plainS)
	}
	if !strings.Contains(plainS, `"cmd":{"type":"string"}`) {
		t.Fatalf("普通 function 工具的 parameters 必须保持原样:\n%s", plainS)
	}

	// 4. 合成名不得等于原名，否则出站无法区分还原。
	if _, dup := byName["apply_patch"]; dup {
		t.Fatalf("custom 工具禁止以原名 apply_patch 发给 CC 上游（会与 Codex 原工具语义冲突）")
	}
}

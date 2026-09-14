package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// TestBuildCCRequest_ToolSearchSynthesized 验证 tool_search 工具被合成成
// type:"function" 且 name 字面量为 "tool_search"（与 Codex 侧 TOOL_SEARCH_TOOL_NAME
// 常量逐字节一致）。
//
// 合成方向固定：入站 type:"tool_search" → CC function("tool_search", description, parameters)
//   → 出站按 function_call 的 name 判定还原成 tool_search_call item。
// 名称一旦漂移（比如写成 toolSearch 或 tool_search_tool），Codex 的
// ToolName::plain("tool_search") 找不到 executor → RespondToModel 而不是 Fatal，
// 但整轮工具调用静默失败。
//
// 负向断言：web_search 必须保持丢弃。web_search 在 Codex 侧无本地 executor
// （tool_spec.rs:44-54 无 execution 字段），CC 侧也没有，合成会造幻影——
// 模型看见工具、尝试调用，但没有任何一方执行。
func TestBuildCCRequest_ToolSearchSynthesized(t *testing.T) {
	// 与 codex-rs/core/src/tools/handlers/tool_search_spec.rs:97-105 完全一致的形状。
	toolSearchRaw := []byte(`{
		"type":"tool_search",
		"execution":"client",
		"description":"# Tool discovery\n\nSearches over deferred tool metadata with BM25",
		"parameters":{
			"type":"object",
			"properties":{"query":{"type":"string","description":"Query"}},
			"required":["query"],
			"additionalProperties":false
		}
	}`)
	webSearchRaw := []byte(`{
		"type":"web_search",
		"external_web_access":false,
		"search_content_types":["text","image"]
	}`)

	req := &schema.InternalRequest{
		Model: "glm-5.2",
		Messages: []schema.InternalMessage{{
			Role: "user",
			Content: json.RawMessage(`"hi"`),
		}},
		Tools: []schema.InternalTool{
			{Type: "function", Function: &schema.InternalFunction{Name: "exec_command"}},
			{Type: "tool_search", Raw: toolSearchRaw},
			{Type: "web_search", Raw: webSearchRaw},
		},
	}

	ccReq := buildCCRequest(req, "https://api.example.com/v1")

	// 期望：2 个工具（普通 function + tool_search 合成）；web_search 被丢弃。
	if len(ccReq.Tools) != 2 {
		t.Fatalf("期望 2 个工具（exec_command + tool_search），实际 %d", len(ccReq.Tools))
	}

	// 找到 tool_search 合成的那个
	var foundToolSearch, foundWebSearch, foundExec bool
	for _, tool := range ccReq.Tools {
		if tool.Type != "function" {
			t.Errorf("所有工具必须 type:\"function\"，实际 type=%q", tool.Type)
			continue
		}
		if tool.Function == nil {
			t.Errorf("tools[] 元素缺 function 字段")
			continue
		}
		switch tool.Function.Name {
		case "tool_search":
			foundToolSearch = true
			// 描述必须原样透传，不改写
			if tool.Function.Description != "# Tool discovery\n\nSearches over deferred tool metadata with BM25" {
				t.Errorf("description 未原样透传，实际 %q", tool.Function.Description)
			}
			if tool.Function.Parameters == nil {
				t.Errorf("tool_search 的 parameters 不能为空（schema 丢了）")
				continue
			}
			// schema 必须保真：required:["query"] + additionalProperties:false
			if _, ok := tool.Function.Parameters["properties"]; !ok {
				t.Errorf("parameters.properties 丢失")
			}
			req, ok := tool.Function.Parameters["required"].([]interface{})
			if !ok || len(req) != 1 || req[0] != "query" {
				t.Errorf("parameters.required 未保真: %v", tool.Function.Parameters["required"])
			}
			addl, _ := tool.Function.Parameters["additionalProperties"].(bool)
			if addl != false {
				t.Errorf("parameters.additionalProperties 未保真: %v", tool.Function.Parameters["additionalProperties"])
			}
		case "exec_command":
			foundExec = true
		case "web_search":
			foundWebSearch = true
		}
	}
	if !foundToolSearch {
		t.Errorf("tool_search 未被合成到 CC tools[]：%+v", ccReq.Tools)
	}
	if !foundExec {
		t.Errorf("普通 function 工具丢失")
	}
	if foundWebSearch {
		t.Errorf("web_search 禁止合成——Codex 侧无本地 executor，合成即幻影")
	}
}

// TestBuildCCRequest_ToolSearchSkippedWhenRawEmpty 验证 tool_search 但 Raw 为空
// 时不进合成分支（走 nUnsupported），避免把空描述合成给 CC 上游。
func TestBuildCCRequest_ToolSearchSkippedWhenRawEmpty(t *testing.T) {
	req := &schema.InternalRequest{
		Model: "m",
		Messages: []schema.InternalMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []schema.InternalTool{
			{Type: "tool_search", Raw: nil},
		},
	}
	ccReq := buildCCRequest(req, "")
	if len(ccReq.Tools) != 0 {
		t.Errorf("tool_search 缺 Raw 时禁止合成，实际 %d 个工具", len(ccReq.Tools))
	}
}

// TestBuildCCRequest_ToolSearchSkippedWhenDescriptionMissing 验证 tool_search 但
// description 为空时同样不进合成分支——合成空描述的工具会让模型误调用。
func TestBuildCCRequest_ToolSearchSkippedWhenDescriptionMissing(t *testing.T) {
	req := &schema.InternalRequest{
		Model: "m",
		Messages: []schema.InternalMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []schema.InternalTool{
			{Type: "tool_search", Raw: json.RawMessage(`{"type":"tool_search","parameters":{"type":"object"}}`)},
		},
	}
	ccReq := buildCCRequest(req, "")
	if len(ccReq.Tools) != 0 {
		t.Errorf("tool_search 缺 description 时禁止合成，实际 %d 个工具", len(ccReq.Tools))
	}
}

// TestBuildCCRequest_OtherUnrecognisedTypesStillDropped 验证未知 type
// （比如 Codex 未来新增）仍然走 nUnsupported 丢弃，且日志可见。
//
// 与 RESPONSES_FILTER_BUILTIN_TOOLS 的约束「禁止按 type 白名单丢弃」的关系：
// 该约束针对的是**入站**翻译器（不允许在 Central Schema 边界丢工具）；
// buildCCRequest 是**出站**边界，按目标协议（CC）能力丢弃是正确的。
// 本测试锁定的是「未知 type 不被误合成」——只有显式识别的 type:"tool_search"
// 才进入合成分支，其他一律丢弃。
func TestBuildCCRequest_OtherUnrecognisedTypesStillDropped(t *testing.T) {
	req := &schema.InternalRequest{
		Model: "m",
		Messages: []schema.InternalMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []schema.InternalTool{
			{Type: "image_generation", Raw: json.RawMessage(`{"type":"image_generation","description":"x","parameters":{"type":"object"}}`)},
			{Type: "custom", Raw: json.RawMessage(`{"type":"custom","name":"apply_patch"}`), IsCustom: true},
		},
	}
	ccReq := buildCCRequest(req, "")
	// 期望：只有 custom 合成的 exec_apply_patch；image_generation 被丢弃。
	if len(ccReq.Tools) != 1 {
		t.Fatalf("期望 1 个工具（custom 合成），实际 %d", len(ccReq.Tools))
	}
	if got := ccReq.Tools[0].Function.Name; got != "exec_apply_patch" {
		t.Errorf("custom 合成名应为 exec_apply_patch，实际 %q", got)
	}
	// 确认 image_generation 没混进来
	b, _ := json.Marshal(ccReq.Tools)
	if strings.Contains(string(b), "image_generation") {
		t.Errorf("image_generation 禁止被合成到 CC tools[]")
	}
}

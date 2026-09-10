package responses

import (
	"context"
	"encoding/json"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// ═══════════════════════════════════════════════════════════════════════════════
//  CUSTOM (freeform) TOOL SUPPORT
//
//  Codex 用 type:"custom" 的工具承载 apply_patch 这类 freeform 工具
//  （codex-rs/tools/src/tool_spec.rs:22 ToolSpec::Freeform → wire "custom"，
//  responses_api.rs:16 FreeformTool{name, description, format:{type,syntax,definition}}）。
//
//  Codex 侧的收发约定（源码为证）：
//  - 出站 item：protocol/models.rs:1121 ResponseItem::CustomToolCall
//      {call_id, name, input: String}  ← input 是裸字符串，不是 JSON 对象
//  - 入站 item：models.rs:857 ResponseInputItem::CustomToolCallOutput
//      {call_id, output}，与 FunctionCallOutput 的 output 编码完全相同
//  - 解析入口：codex-api/src/sse/responses.rs:509-512 对 output_item.added/done
//    直接 serde_json::from_value::<ResponseItem>，因此 item.type 写 "custom_tool_call"
//    即被识别；response.completed 的 output[] 不被读取（ResponseCompleted 只解析
//    id/usage/end_turn），所以工具执行完全依赖 output_item.done。
//
//  @AI_GUARD: RESPONSES_CUSTOM_TOOL - custom 工具的双向桥接
//  @CONSTRAINT: CC 上游的 tools[] 只认 type:"function"，无法表达 freeform 语法。
//    映射方向必须是「入站 custom → 出站给 CC 时合成 JSON 函数 → 返回给 Codex 时
//    还原成 custom_tool_call」，不能把 type:"custom" 原样发给 CC 上游（400）。
//  @RELATED: server/gateway.go buildCCRequest（合成 function）、schema/internal.go
//    InternalTool.IsCustom、translator.go inputToMessages（custom_tool_call 历史条目）
//  @REASON: v0.2.131 之前 translator.go 丢弃所有 type!="function" 的工具，Codex 每次
//    请求固定丢 4 个（tools=14→tools=10），其中 type:"custom" 的 apply_patch 是它唯一
//    能改文件的工具——模型被要求"用 apply_patch 写文件"却拿不到该工具，只能退化成
//    exec_command 或纯散文。
// ═══════════════════════════════════════════════════════════════════════════════

// customToolCtxKey 把「本轮哪些 tool_call 名对应 custom 工具」传给 TranslateStream。
// TranslateStream 的签名是接口契约（internal/translator/interfaces.go），
// 这里用 context 携带，避免为一条可选信息改动四个翻译器的公共接口。
type customToolCtxKey struct{}

// WithCustomTools 把 custom 工具名表挂到 ctx 上。TranslateStream 据此把
// function_call 还原成 custom_tool_call。
func WithCustomTools(ctx context.Context, names map[string]bool) context.Context {
	return context.WithValue(ctx, customToolCtxKey{}, names)
}

// customToolNames 取 ctx 上的 custom 工具名表；没有则返回 nil。
func customToolNames(ctx context.Context) map[string]bool {
	if ctx == nil {
		return nil
	}
	if m, ok := ctx.Value(customToolCtxKey{}).(map[string]bool); ok {
		return m
	}
	return nil
}

// CollectCustomToolNames 从中枢工具表收集 custom 工具名，供调用方
// 通过 WithCustomTools 挂到 ctx 上传给 TranslateStream。
func CollectCustomToolNames(tools []schema.InternalTool) map[string]bool {
	names := make(map[string]bool)
	for _, t := range tools {
		if t.IsCustom && t.Function != nil && t.Function.Name != "" {
			names[t.Function.Name] = true
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// ccCustomToolDescription 给 CC 上游合成的 function 工具描述。
// CC 只能看到 JSON schema，看不到 freeform 语法；但模型仍然需要知道
// "把补丁放进 input 字段"，否则它会返回一个空参数或随手编的 JSON。
func ccCustomToolDescription(name string, raw json.RawMessage) string {
	desc := ""
	var obj struct {
		Description string `json:"description"`
	}
	if raw != nil {
		json.Unmarshal(raw, &obj)
		desc = obj.Description
	}
	if desc == "" {
		desc = "Invoke the freeform tool " + name + "."
	}
	return desc + "\n\nThe tool input is a single raw text payload. Put the ENTIRE tool payload in the \"input\" string field verbatim, exactly as you would write it directly to the tool. Do NOT wrap it in extra JSON, do NOT escape it, do NOT add any surrounding commentary."
}

// customToolParameters 给 CC 上游合成的 function 参数 schema。
// 刻意保持最小：只有 required 的 input 字符串。加 extra_body 会诱导模型
// 生成 schema 之外的字段。
func customToolParameters() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"properties":           map[string]interface{}{"input": map[string]interface{}{"type": "string"}},
		"required":             []string{"input"},
		"additionalProperties": false,
	}
}

// unwrapCCCustomArguments 把 CC 上游对 custom 工具合成的 arguments JSON
// （如 {"input":"*** Begin Patch\n*** Add File: x"}）还原成 freeform 工具需要的
// 裸字符串。
//
// 返回 (裸字符串, 是否走了解包路径)。
// 解包失败时原样返回 arguments——freeform 工具拿到 JSON 文本比拿到空串更能被
// 模型修正，而空串会让 Codex 直接判失败。
func unwrapCCCustomArguments(args string) (string, bool) {
	trimmed := args
	if len(trimmed) == 0 {
		return args, false
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return args, false
	}
	if raw, ok := obj["input"].(string); ok {
		return raw, true
	}
	return args, false
}

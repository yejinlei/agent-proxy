package responses

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/agent-proxy/agent-proxy/internal/protocol/chatcompletion"
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
//    映射方向必须是「入站 custom → 出站给 CC 时合成 exec_<原名> 的 JSON 函数
//    → 返回给 Codex 时还原成 custom_tool_call（name 回原名、input 是裸串）」，
//    不能把 type:"custom" 原样发给 CC 上游（400）。
//    合成名→原名的映射必须显式携带（流式走 WithCustomTools/ctx，非流式走
//    InternalResponse.CustomToolNames），禁止从 IsCustom 工具的 Function.Name 取——
//    Function 字段对 custom 工具永远为空（见 translator.go
//    @AI_GUARD: RESPONSES_CUSTOM_TOOL_NO_SHIM）。
// @RELATED: server/gateway.go buildCCRequest（ExecPatchTool 合成）、schema/internal.go
//   InternalTool.IsCustom / InternalResponse.CustomToolNames、translator.go
//   inputToMessages（custom_tool_call 历史条目）
// @REASON: v0.2.131 之前 translator.go 丢弃所有 type!="function" 的工具，Codex 每次
//   请求固定丢 4 个（tools=14→tools=10），其中 type:"custom" 的 apply_patch 是它唯一
//   能改文件的工具——模型被要求"用 apply_patch 写文件"却拿不到该工具，只能退化成
//   exec_command 或纯散文。v0.2.132 改成在入站翻译器里给 custom 工具填
//   Function={input:string} + 合成描述，结果 Function 会被 buildCCRequest 当普通
//   函数原样转发，且合成描述串到了 7 个普通 function 工具上
//   （agent-proxy-9093.log：shim 后缀计数 7、custom_tool_call 计数 0、apply_patch
//   仅出现在系统提示里）——v0.2.133 起合成只在 buildCCRequest 一处发生。
// ═══════════════════════════════════════════════════════════════════════════════

// execPatchToolNameMap 把「本轮送进 CC 上游的合成工具名」映射回「Codex 原始 custom 工具名」。
// TranslateStream 据此把 function_call 还原成 custom_tool_call（出站 name 必须是原名）。
// TranslateStream 的签名是接口契约（internal/translator/interfaces.go），
// 这里用 context 携带，避免为一条可选信息改动四个翻译器的公共接口。
type execPatchToolNameMap struct{}

// WithCustomTools 把合成名→原名映射挂到 ctx 上。TranslateStream 据此把
// 合成工具的 function_call 还原成原名 + custom_tool_call。
func WithCustomTools(ctx context.Context, names map[string]string) context.Context {
	return context.WithValue(ctx, execPatchToolNameMap{}, names)
}

// customToolNames 取 ctx 上的合成名→原名映射；没有则返回 nil。
func customToolNames(ctx context.Context) map[string]string {
	if ctx == nil {
		return nil
	}
	if m, ok := ctx.Value(execPatchToolNameMap{}).(map[string]string); ok {
		return m
	}
	return nil
}

// toolNameFromRaw 取工具定义里的原工具名，不做任何加工。
func toolNameFromRaw(raw json.RawMessage) string {
	var obj struct {
		Name string `json:"name"`
	}
	if raw != nil {
		json.Unmarshal(raw, &obj)
	}
	return obj.Name
}

// execPatchToolNameFor 由 custom 工具原定义派生「代理代为执行」合成工具在 CC 上游的
// 工具名。派生（而非固定常量）是为了让多个 custom 工具各自拥有独立入口——
// 出站必须按原名还原成 custom_tool_call，一个固定名只能承载一个 custom 工具。
// 派生规则对同一原定义稳定，因此跨轮次不会抖动；非法字符折叠成 _ 。
func execPatchToolNameFor(raw json.RawMessage) string {
	name := toolNameFromRaw(raw)
	var sb strings.Builder
	sb.WriteString(execPatchToolPrefix)
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	if sb.Len() == len(execPatchToolPrefix) {
		// 原定义里没有可派生的名字（畸形工具），退化成固定名，至少工具还能用。
		return execPatchToolPrefix + "freeform"
	}
	return sb.String()
}

// CollectCustomToolNames 从中枢工具表收集「合成工具名 → Codex 原 custom 工具名」映射，
// 供调用方通过 WithCustomTools（流式）或 InternalResponse.CustomToolNames（非流式）
// 传给 TranslateStream / TranslateResponse。没有 custom 工具时返回 nil。
//
// 注意：这里必须用 execPatchToolNameFor 派生的名做 key，不能用 IsCustom 工具的
// InternalTool.Function.Name——Function 字段已禁止给 custom 工具填写
// （translator.go @AI_GUARD: RESPONSES_CUSTOM_TOOL_NO_SHIM）。
func CollectCustomToolNames(tools []schema.InternalTool) map[string]string {
	names := make(map[string]string)
	for _, t := range tools {
		if !t.IsCustom || len(t.Raw) == 0 {
			continue
		}
		orig := toolNameFromRaw(t.Raw)
		if orig == "" {
			continue
		}
		names[execPatchToolNameFor(t.Raw)] = orig
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// toolDescriptionFromRaw 取工具定义里的 description 原文，不做任何加工。
// function 工具入站时用它填 InternalTool.Function.Description——不能走
// ccCustomToolDescription，那个函数会追加 freeform 工具的裸文本说明，
// 加在普通 function 工具上会把它们误导成「无参数、只收裸文本」。
func toolDescriptionFromRaw(raw json.RawMessage) string {
	var obj struct {
		Description string `json:"description"`
	}
	if raw != nil {
		json.Unmarshal(raw, &obj)
	}
	return obj.Description
}

// ccCustomToolDescription 给 CC 上游合成的 function 工具描述。
// CC 只能看到 JSON schema，看不到 freeform 语法；但模型仍然需要知道
// "把补丁放进 input 字段"，否则它会返回一个空参数或随手编的 JSON。
func ccCustomToolDescription(name string, raw json.RawMessage) string {
	desc := toolDescriptionFromRaw(raw)
	if desc == "" {
		desc = "Invoke the freeform tool " + name + "."
	}
	return desc + "\n\n" + customRawInputInstruction
}

// customRawInputInstruction 是 freeform 工具合成参数说明的固定后缀。
const customRawInputInstruction = "The tool input is a single raw text payload. Put the ENTIRE tool payload in the \"input\" string field verbatim, exactly as you would write it directly to the tool. Do NOT wrap it in extra JSON, do NOT escape it, do NOT add any surrounding commentary."

// execPatchToolPrefix 是「代理代为执行补丁」合成工具在 CC 上游的命名前缀。
// 完整的工具名是 execPatchToolPrefix + 原 custom 工具名（见 execPatchToolNameFor），
// 这样每个 custom 工具都有自己的入口，出站才能按原名还原成 custom_tool_call。
const execPatchToolPrefix = "exec_"

// ExecPatchTool 把 Codex 的 freeform 工具包装成 CC 上游可调用形状的函数：
// 参数是 {input:<裸串>}，工具名由原工具名派生（exec_<原名>），出站按该派生名还原成
// custom_tool_call（name 回到原名、input 是裸串）。
//
// @REASON: CC 上游没有本地 executor 的 function 工具——模型调用只是回显参数，
// 补丁不会落盘。把 freeform 工具原样/以合成参数发给上游是能力错误，v0.2.132
// 就是这么踩的：apply_patch 被调 0 次，写操作全部退化成 exec_command 内联
// here-string（agent-proxy-9093.log）。
func ExecPatchTool(raw json.RawMessage) chatcompletion.Tool {
	name := execPatchToolNameFor(raw)
	return chatcompletion.Tool{
		Type: "function",
		Function: &chatcompletion.FunctionDef{
			Name:        name,
			Description: ccCustomToolDescription(toolNameFromRaw(raw), raw),
			Parameters:  customToolParameters(),
		},
	}
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

package responses

import (
	"context"
	"encoding/json"
	"log"

	"github.com/agent-proxy/agent-proxy/internal/protocol/chatcompletion"
	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// ═══════════════════════════════════════════════════════════════════════════════
//  NAMESPACE TOOL SUPPORT（Codex MCP / multi_agent 命名空间）
//
//  Codex 用 type:"namespace" 的工具承载命名空间化的 function 工具
//  （codex-rs/tools/src/responses_api.rs:58 ResponsesApiNamespace{
//  name, description, tools: Vec<ResponsesApiNamespaceTool>}，工具项可以是
//  Function(ResponsesApiTool) 或 Custom(FreeformTool)）。
//
//  与 custom 工具的关键区别——命名空间**不是另一种工具类型，只是加了 namespace 字段**：
//  - 出站 item：protocol/models.rs FunctionCall{name, namespace: Option<String>,
//    arguments}，type 仍是 "function_call"，name 是原工具名，namespace 是命名空间名
//    （core/tests/common/responses.rs ev_function_call_with_namespace 为证）。
//    所以这里**不需要改 CC 侧的工具名**，只需出站时加一个字段。
//  - 工具结果：FunctionCallOutput 只按 call_id 配对（models.rs:1101 call_id 可选、
//    namespace 可选），代理生成的 call_id 全程可控，配对不受展平影响。
//
//  @AI_GUARD: RESPONSES_NAMESPACE_TOOL - namespace 工具的双向桥接
//  @CONSTRAINT: CC 上游的 tools[] 只认 type:"function"，无法表达命名空间，
//    因此入站 namespace 只能**展平**成顶层 function 转发：
//      合成名 = <namespace>_<toolname>，schema 原样透传；
//      出站 function_call item 还原成 name:<原工具名> + namespace:<命名空间名>。
//    展平后的工具名与命名空间名的映射必须显式携带（流式走 WithNamespaceTools/ctx，
//    非流式走 InternalResponse.NamespaceTools），禁止按前缀字符串反推——
//    前缀不是无损的（工具名本身可以含下划线），反推会张冠李戴。
//    同时禁止把 namespace 里的工具**提升为顶层 function** 转发：出站 item 只能写
//    type:"function_call"，不带 namespace 字段的上游工具调用 Codex 会当成顶层函数
//    执行，而顶层并不存在同名工具 → 整轮静默失败。
// @RELATED: server/gateway.go buildCCRequest（NamespaceTool 展平）、schema/internal.go
//   InternalTool（INTERNAL_TOOL_RAW）、InternalResponse.NamespaceTools、
//   translator.go TranslateStream（funcCallState.Namespace / customFCItem）、
//   translator.go TranslateResponse（非流式 output item）
// @REASON: v0.2.132 之前 buildCCRequest 只按 Function!=nil 转发，namespace 工具
//   （mcp__codegraph / mcp__zvec_grep / multi_agent_v1）每次请求固定丢掉 3 个
//   （tools=14→10，agent-proxy-9091/9092/9093.log）。Codex 系统提示明确要求用这些
//   MCP 工具做证据检索，模型拿不到只能退化到 exec_command 里跑命令行。
//   v0.2.134 起丢弃清单可观测（日志打出具体工具名），但从未被修好——
//   09-12 主编码轮次 122 items / tools=14 仍在 func_calls=0 下 finish_reason=stop。
// ═══════════════════════════════════════════════════════════════════════════════

// namespaceToolPrefix 是展平工具名与命名空间名之间的分隔符。
const namespaceToolPrefix = "_"

// nsToolNameMap 把「本轮送进 CC 上游的展平工具名」映射回「<命名空间名, 原工具名>」。
// TranslateStream 据此在 function_call item 上补 namespace 字段并还原工具名。
// TranslateStream 的签名是接口契约（internal/translator/interfaces.go），
// 这里用 context 携带，避免为一条可选信息改动四个翻译器的公共接口。
type nsToolNameMap struct{}

// NamespaceEntry 是「展平名 → 命名空间 + 原工具名」的映射值。
// 直接复用 schema 的中枢类型：非流式路径要把它塞进 InternalResponse.NamespaceTools，
// 而 schema 不能 import responses，所以载体类型定义在 schema 侧。
type NamespaceEntry = schema.InternalNamespaceToolRef

// WithNamespaceTools 把展平名→(命名空间, 原工具名)映射挂到 ctx 上，供
// TranslateStream 把 function_call 还原成带 namespace 字段的 item。
func WithNamespaceTools(ctx context.Context, names map[string]NamespaceEntry) context.Context {
	return context.WithValue(ctx, nsToolNameMap{}, names)
}

// namespaceTools 取 ctx 上的展平工具映射；没有则返回 nil。
func namespaceTools(ctx context.Context) map[string]NamespaceEntry {
	if ctx == nil {
		return nil
	}
	if m, ok := ctx.Value(nsToolNameMap{}).(map[string]NamespaceEntry); ok {
		return m
	}
	return nil
}

// namespaceInfoFromRaw 从工具原始 JSON 里取命名空间名，不做任何加工。
func namespaceInfoFromRaw(raw json.RawMessage) string {
	var obj struct {
		Name string `json:"name"`
	}
	if raw != nil {
		json.Unmarshal(raw, &obj)
	}
	return obj.Name
}

// namespaceSubToolInfo 从命名空间内的单个工具项原始 JSON 里取工具名。
// 两种形态都要兼容：
//   - Codex 内联形态 {"type":"function","name":"x","parameters":{...}}
//   - CC 嵌套形态     {"type":"function","function":{"name":"x","parameters":{...}}}
// 后者在 translator.go Tool.UnmarshalJSON 已有同样处理，此处保持一致。
func namespaceSubToolInfo(raw json.RawMessage) string {
	var obj struct {
		Name     string `json:"name"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if raw != nil {
		json.Unmarshal(raw, &obj)
	}
	if obj.Function != nil && obj.Function.Name != "" {
		return obj.Function.Name
	}
	return obj.Name
}

// namespaceToolNameFor 由命名空间名 + 原工具名派生展平后的 CC 工具名。
// 派生（而非固定常量）是为了让多个命名空间的同名工具各自拥有独立入口——
// 出站必须按原名 + 命名空间还原，一个固定名只能承载一个工具。
// 派生规则对同一工具定义稳定，因此跨轮次不会抖动。
func namespaceToolNameFor(nsName, toolName string) string {
	return nsName + namespaceToolPrefix + toolName
}

// namespaceParametersFromRaw 取命名空间内单个工具的 parameters 原始 JSON。
// 取不到（畸形工具）时返回空对象——CC 上游允许 parameters 为空。
func namespaceParametersFromRaw(raw json.RawMessage) map[string]interface{} {
	var obj struct {
		Parameters  json.RawMessage `json:"parameters"`
		Function    *struct {
			Parameters json.RawMessage `json:"parameters"`
		} `json:"function"`
		Strict bool `json:"strict"`
	}
	if raw != nil {
		json.Unmarshal(raw, &obj)
	}
	var src json.RawMessage
	if obj.Function != nil && len(obj.Function.Parameters) > 0 {
		src = obj.Function.Parameters
	} else {
		src = obj.Parameters
	}
	var params map[string]interface{}
	if len(src) > 0 {
		if err := json.Unmarshal(src, &params); err != nil {
			params = nil
		}
	}
	if params == nil {
		params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	return params
}

// namespaceDescriptionFromRaw 取命名空间内单个工具的 description 原文，不做任何加工。
func namespaceDescriptionFromRaw(raw json.RawMessage) string {
	var obj struct {
		Description string `json:"description"`
		Function    *struct {
			Description string `json:"description"`
		} `json:"function"`
	}
	if raw != nil {
		json.Unmarshal(raw, &obj)
	}
	if obj.Function != nil {
		return obj.Function.Description
	}
	return obj.Description
}

// NamespaceTool 把命名空间内单个 function 工具展平成 CC 上游可调用形状的顶层函数：
// 工具名 = <namespace>_<toolname>，schema 与 description 原样透传。
// 出站按 CollectNamespaceToolNames 的同名映射还原成 name:<原工具名> + namespace:<命名空间名>。
//
// @REASON: CC 上游不认 type:"namespace"，原样转发会 400；只提升成顶层 function
// 又不行——出站 item 不带 namespace 字段，Codex 会把调用当成顶层函数执行而失败。
// 展平是唯一能同时满足「CC 能收」「Codex 能认领」的形状。
func NamespaceTool(nsName string, toolRaw json.RawMessage) chatcompletion.Tool {
	toolName := namespaceSubToolInfo(toolRaw)
	return chatcompletion.Tool{
		Type: "function",
		Function: &chatcompletion.FunctionDef{
			Name:        namespaceToolNameFor(nsName, toolName),
			Description: namespaceDescriptionFromRaw(toolRaw),
			Parameters:  namespaceParametersFromRaw(toolRaw),
		},
	}
}

// namespaceSubTool 是命名空间内的单个 function 工具展平结果：
// Tool 是转发给 CC 上游的形状，OriginalName 是 Codex 侧的**原工具名**。
//
// @AI_GUARD: RESPONSES_NAMESPACE_TOOL - 原工具名必须由子项 JSON 直接读出，禁止从展平名反推
// @CONSTRAINT: 展平名 = <namespace>_<toolname>，而命名空间名本身可以含下划线
//   （Codex MCP 命名空间的实测形态是 mcp__codegraph / mcp__zvec_grep），
//   按第一个分隔符截前缀会得到 "codegraph" 而非 "mcp__codegraph"，出站 namespace
//   字段就指到了不存在的命名空间上，整轮工具调用静默失败。
//   所以原工具名必须随展平一起携带（本结构体 / NamespaceEntry），不得事后派生。
// @REASON: 展平是可逆映射，不是命名约定；约定式反推对含下划线的前缀必然失真。
type NamespaceSubTool struct {
	Tool         chatcompletion.Tool
	OriginalName string
}

// namespaceSubTools 展平**整个 namespace 工具**，返回该命名空间内的 function 工具列表。
// 非 function 子项（Codex 的 namespace 允许内嵌 custom freeform 工具）没有 CC 等价表达，
// 必须打日志可见而不是静默丢弃。
func NamespaceSubTools(nsRaw json.RawMessage) []NamespaceSubTool {
	var ns struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if nsRaw != nil {
		json.Unmarshal(nsRaw, &ns)
	}
	nsName := namespaceInfoFromRaw(nsRaw)
	var out []NamespaceSubTool
	for _, t := range ns.Tools {
		var head struct {
			Type string `json:"type"`
		}
		if t != nil {
			json.Unmarshal(t, &head)
		}
		if head.Type != "" && head.Type != "function" {
			// @AI_GUARD: RESPONSES_NAMESPACE_TOOL - 命名空间内的非 function 子项必须可观测
			// @CONSTRAINT: 与 buildCCRequest 对顶层非 function 工具的丢弃一致——
			//   只打日志，不静默。命名空间内的 custom（freeform）工具同样没有 CC 本地
			//   executor，不能像顶层 custom 那样合成 exec_ 前缀转发。
			// @REASON: 丢工具是"模型被要求用它却拿不到"这一类静默故障的根源；
			//   v0.2.132 之前连丢了哪些工具都看不到，只能靠猜。
			log.Printf("[CODEX-DEBUG] namespace %q: sub-tool type=%q (name=%q) has no CC equivalent and is not sent upstream",
				nsName, head.Type, namespaceSubToolInfo(t))
			continue
		}
		toolName := namespaceSubToolInfo(t)
		if toolName == "" {
			log.Printf("[CODEX-DEBUG] namespace %q: dropped sub-tool with empty name", nsName)
			continue
		}
		out = append(out, NamespaceSubTool{Tool: NamespaceTool(nsName, t), OriginalName: toolName})
	}
	return out
}

// CollectNamespaceToolNames 从中枢工具表收集「展平工具名 → (命名空间, 原工具名)」映射，
// 供调用方通过 WithNamespaceTools（流式）或 InternalResponse.NamespaceTools（非流式）
// 传给 TranslateStream / TranslateResponse。没有 namespace 工具时返回 nil。
//
// 冲突处理：同一展平名被两个命名空间占用时保留**第一个**并打日志——
// 后到的会被覆盖，出站映射就不唯一了。这类冲突只能靠 Codex 侧改命名空间名解决，
// 代理无法自纠，必须让它可见。
func CollectNamespaceToolNames(tools []schema.InternalTool) map[string]NamespaceEntry {
	names := make(map[string]NamespaceEntry)
	for _, t := range tools {
		if t.Type != "namespace" || len(t.Raw) == 0 {
			continue
		}
		nsName := namespaceInfoFromRaw(t.Raw)
		if nsName == "" {
			continue
		}
		for _, st := range NamespaceSubTools(t.Raw) {
			if st.Tool.Function == nil {
				continue
			}
			flat := st.Tool.Function.Name
			if _, dup := names[flat]; dup {
				log.Printf("[CODEX-DEBUG] namespace tool name conflict: %q already mapped, duplicate from namespace %q dropped",
					flat, nsName)
				continue
			}
			names[flat] = NamespaceEntry{Namespace: nsName, ToolName: st.OriginalName}
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}


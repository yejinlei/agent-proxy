package responses

import (
	"encoding/json"
	"strings"
)

// ToolSearchTypeName 是 Responses 入站工具 type 的字段值。
// Codex 在 codex-rs/tools/src/tool_spec.rs 用 #[serde(rename = "tool_search")]
// 声明该变体，代理必须按字面量匹配。
const ToolSearchTypeName = "tool_search"

// ToolSearchFunctionName 是发往 CC 上游的合成函数名，必须与 Codex 侧
// TOOL_SEARCH_TOOL_NAME 常量逐字节一致。
//
// Codex 侧：codex-rs/tools/src/tool_discovery.rs:6
//   pub const TOOL_SEARCH_TOOL_NAME: &str = "tool_search";
// handler 注册 ToolName::plain(TOOL_SEARCH_TOOL_NAME)，Router 按
// ToolName::plain("tool_search") 查找 executor。合成名写成任何其他值（含带
// namespace 前缀）都会让出站改写的 item 落到 Codex 不认识的函数上。
const ToolSearchFunctionName = "tool_search"

// ToolSearchParamsView 是入站 type:"tool_search" 工具在 Central Schema 侧的
// 最小视图。Codex 的完整结构还有 execution:"client" 字段（表明客户端本地
// 执行），代理不需要——出站改写时固定写 "client"。
type ToolSearchParamsView struct {
	Description  string                 `json:"description"`
	Parameters   map[string]interface{} `json:"parameters"`
}

// ToolSearchParams 从入站工具的原始 JSON 解出 description 与 parameters。
// 返回值：解析成功且 description 非空时 ok=true；否则调用方按 nUnsupported 丢弃，
// 不得把空 description 合成给 CC 上游（模型会拿到无意义工具说明并可能误调用）。
//
// @AI_GUARD: RESPONSES_TOOL_SEARCH_PARAMS - 形状探测只做校验、不改写
// @CONSTRAINT: Parameters 必须原样透传入站 JSON，不得重建/合并/降级为 {}。
//   Codex 发的 schema 里有 required:["query"] 与 additionalProperties:false
//   （codex-rs/core/src/tools/handlers/tool_search_spec.rs:97-105），
//   降级成 {} 会让模型以为工具无参数 → 空 query 调用 → Codex RespondToModel
//   "query must not be empty"。
// @REASON: v0.2.142 前 buildCCRequest 把 tool_search 整条丢进 nUnsupported，
//   模型完全看不见该工具。修好后必须保证 schema 保真，否则从「看不见」退化成
//   「看见但没用」，比之前更难归因。
// @RELATED: server/gateway.go buildCCRequest（CC_TOOL_SEARCH_SYNTHESIS）、
//   translator.go TranslateResponse / TranslateStream（RESPONSES_TOOL_SEARCH_BRIDGE）
func ToolSearchParams(raw json.RawMessage) (*ToolSearchParamsView, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var p ToolSearchParamsView
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, false
	}
	if p.Description == "" {
		return nil, false
	}
	return &p, true
}

// IsToolSearchArgsValid 判定出站改写的候选 arguments JSON 是否值得转成
// tool_search_call item。
//
// Codex 侧 SearchToolCallParams 只有两个字段：query:String（必填）+
// limit:Option<usize>（codex-rs/protocol/src/models.rs:2063-2067）。
// Router 反序列化失败会给模型回 "failed to parse tool_search arguments"，
// 空 query 会给模型回 "query must not be empty"——两种都是 RespondToModel，
// 不致命但浪费一轮。提前拦掉比事后兜底更省。
//
// 返回 (valid, cleaned):
//   - valid=false：arguments 不是对象、query 缺失或非字符串、或 limit 非法。
//     调用方必须保留原 function_call item 并打日志，不得静默改写成 tool_search_call
//     ——那等于把一个 Codex 一定拒绝的形状换成另一个 Codex 一定拒绝的形状。
//   - cleaned：合法时返回的规范化对象（只含 query 和（若合法）limit），
//     保证不夹带任何 SearchToolCallParams 不认识的字段。
//
// @AI_GUARD: RESPONSES_TOOL_SEARCH_ARGS - 出站改写前的形状校验
// @CONSTRAINT: 拒绝的 arguments 必须由调用方**原样保留为 function_call**，
//   不允许回退成 "无参 tool_search_call"（那会让 Codex 报 query 为空，
//   比保留 function_call 更糟：function_call 至少能让模型看见自己传错了什么）。
// @REASON: 上游 CC 的合成 schema 只约束 {query:string}，模型可能传出
//   {"query":123} / {"q":"x"} / {"query":"x","bogus":1} 等变形。
// @RELATED: translator.go TranslateResponse / TranslateStream（出站改写）
func IsToolSearchArgsValid(argsJSON string) (bool, map[string]interface{}) {
	var obj map[string]interface{}
	if argsJSON == "" {
		return false, nil
	}
	if err := json.Unmarshal([]byte(argsJSON), &obj); err != nil {
		return false, nil
	}
	q, ok := obj["query"]
	if !ok {
		return false, nil
	}
	qs, ok := q.(string)
	if !ok {
		return false, nil
	}
	if strings.TrimSpace(qs) == "" {
		// 与 Codex 的 RespondToModel 语义一致（tool_search.rs:216 对 query 做 trim，
		// 空串报 "query must not be empty"）：空白 query 同样拒绝，
		// 避免改写出一个 Codex 必然回错的 item。
		return false, nil
	}
	cleaned := map[string]interface{}{"query": qs}
	if v, has := obj["limit"]; has {
		switch n := v.(type) {
		case float64:
			// JSON 数字解成 float64；必须是正的整数值才可能是合法 usize。
			if n > 0 && n == float64(int64(n)) && int64(n) < 1<<31 {
				cleaned["limit"] = int64(n)
			}
			// 非正或非整数：直接丢弃 limit 字段而非拒绝整个调用——
			// Codex 侧 limit 是 Option，缺失即走 TOOL_SEARCH_DEFAULT_LIMIT，
			// 是合法降级路径。
		default:
			// 字符串/布尔/对象都不是 usize，同样丢弃。
		}
	}
	return true, cleaned
}

// ToolSearchArgsCleanedJSON 返回出站 tool_search_call.arguments 的规范化 JSON
// 字符串。
//
// 与 IsToolSearchArgsValid 的 map 返回值相比，本函数直接给出**字节级确定**的
// 形状：Go 的 json.Marshal 对 map 按 key 字母序输出，两条出站路径（流式
// output_item.done / 非流式 output[]）各自 Marshal 一次 map，虽然结果一致，但
// 依赖这个隐式排序；直接用同一份字符串更明确，也保证两条路径逐字节可比。
//
// @AI_GUARD: RESPONSES_TOOL_SEARCH_ARGS - 出站 arguments 形状的唯一出口
// @CONSTRAINT: 必须是**对象**，不是字符串——codex-rs/protocol/src/models.rs:1088
//   ToolSearchCall.arguments 是 serde_json::Value 对象，与 FunctionCall 的
//   arguments:String 相反。包成字符串会让 Codex 的 ResponseItem 反序列化失败，
//   output_item.done 静默丢弃 → 工具不执行、不重试。
// @CONSTRAINT: 空串（无参数）禁止调用本函数——空串会被序列化成 "{}"，
//   Codex 侧 SearchToolCallParams.query 缺失 → RespondToModel。
//   调用方必须先用 IsToolSearchArgsValid 判定，不通过时保留 function_call。
// @RELATED: IsToolSearchArgsValid、translator.go TranslateResponse / TranslateStream
func ToolSearchArgsCleanedJSON(argsJSON string) string {
	_, cleaned := IsToolSearchArgsValid(argsJSON)
	b, _ := json.Marshal(cleaned)
	return string(b)
}

package schema

import (
	"encoding/json"
	"time"
)

// ═══════════════════════════════════════════════════════════════════════════════
//  通用内部消息模型 (CENTRAL SCHEMA)
//
//  设计原则：
//  1. 任何协议都可无损翻译为此模型，再翻译回任意协议（理论上）
//  2. 用 interface{} / json.RawMessage 保留原始结构，避免信息丢失
//  3. 所有兼容性问题在 Translator 层显式处理，不在 Schema 层
// ═══════════════════════════════════════════════════════════════════════════════

// ─── Role 映射 ────────────────────────────────────────────────────────
// 各协议角色差异（翻译器必须处理）：
//   CC        → user  | assistant | system | tool
//   Responses → user  | assistant (system 分离到 instructions)
//   Anthropic → user  | assistant | system 必须拆到顶层 system 字段！
//   Gemini    → user  | model     | systemInstruction 顶层字段
//
// 关键规则：
//  - 所有 role:"system" 消息统一提取到系统提示字段，不进入 messages 数组
//  - Gemini 的 role:"model" 翻译回 user 端时映射为 role:"assistant"
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ─── 通用消息 ─────────────────────────────────────────────────────────

// InternalMessage 中枢消息对象
//
// Content 用 json.RawMessage 而非具体类型，原因：
//   - CC 的 content 可以是 string 或 []ContentBlock
//   - Responses/Anthropic 的 content 是 []ContentBlock（结构不同）
//   - Gemini 的 parts 是 []Part（又是另一套结构）
// 在 Central Schema 保留原始 JSON 最安全，翻译时按需解组
type InternalMessage struct {
	Role     Role               `json:"role"`
	Content  json.RawMessage    `json:"content,omitempty"`
	// ContentBlocks 存储入站翻译器解析出的多模态内容块（text + image），
	// 出站时优先使用此字段还原图片内容块，确保图片不丢失。纯文本消息为 nil。
	ContentBlocks []InternalContentBlock `json:"content_blocks,omitempty"`
	ToolCalls   []InternalToolCall `json:"tool_calls,omitempty"`
	ToolCallID  string            `json:"tool_call_id,omitempty"`
	Name        string            `json:"name,omitempty"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
}

// ─── 内容块 (多模态) ─────────────────────────────────────────────────
//
// 各协议内容块差异：
//   CC:       content: [{type:"image_url", image_url:{url:"data:image/..."}}]
//   Responses:content: [{type:"image", source:{type:"base64", data:"...", media_type:"..."}}]
//   Anthropic:content: [{type:"image", source:{type:"base64", data:"...", media_type:"..."}}]
//   Gemini:   parts:  [{inline_data:{data:"...", mime_type:"..."}}]
//
// 统一策略：InternalContentBlock 保留通用字段 + RawExtension 兜底协议特有字段
type InternalContentBlock struct {
	Type     string          `json:"type"` // "text" | "image" | "audio" | "file"
	Text     string          `json:"text"`
	// Image fields
	Data      string `json:"data,omitempty"`     // base64 编码
	MediaType string `json:"media_type,omitempty"`
	URL       string `json:"url,omitempty"`      // 外部 URL
	// File fields (Gemini file_data)
	FileName  string `json:"file_name,omitempty"`
	FileURI   string `json:"file_uri,omitempty"`
	// 协议特有原始字段（兜底用，防止信息丢失）
	RawExtension json.RawMessage `json:"_raw,omitempty"`
}

// ─── Tool / Function Calling ────────────────────────────────────────
//
// 各协议工具调用差异（最重要也最容易出错的部分）：
//
// 定义（入参）：
//   CC:        tools: [{type:"function", function:{name, description, parameters}}]
//   Responses: tools: [{type:"function", name, parameters}]         ← 无 description 字段
//   Anthropic: tools: [{name, description, input_schema}]           ← input_schema 非 parameters
//   Gemini:    tools: [{functionDeclarations:[{name, description, parameters}]}]
//
// 调用（出参）：
//   CC:        tool_calls: [{id, type:"function", function:{name, arguments:"{json}"}}]
//   Responses: tool_calls: [{id, type:"function", name, input:{...}}]  ← input 是对象非字符串！
//   Anthropic: content blocks: [{type:"tool_use", id, name, input:{...}}]  ← 不是 tool_calls 字段
//   Gemini:    parts: [{functionCall:{name, args:{...}}}]           ← args 是对象非字符串
//
// 返回（tool 结果）：
//   CC:        messages: [{role:"tool", tool_call_id, content:"result"}]
//   Responses: input: [{type:"message", role:"user", content:[{type:"tool_result", ...}]}]
//   Anthropic: messages: [{role:"user", content:[{type:"tool_result", tool_use_id, content}]}]
//   Gemini:    contents: [{role:"user", parts:[{functionResponse:{name, response:{result}}}]}]
//
// 关键兼容规则：
//  1. arguments (CC) 是 JSON 字符串 ↔ input/args (其他) 是 JSON 对象 → 需要 Marshal/Unmarshal
//  2. Responses/Anthropic 的 tool_call ID 命名不同（responses_xxx / toolu_xxx）
//  3. Anthropic 把 tool_call 混在 content blocks 里，不是独立字段 → 解析时必须扫描所有 content
//  4. Gemini 没有 tool_call_id 概念 → 需要网关层生成唯一 ID 映射
type InternalTool struct {
	Type        string                 `json:"type"`      // "function"
	Function    *InternalFunction      `json:"function"`
	// Anthropic 用 input_schema 而非 parameters
	InputSchema map[string]interface{} `json:"input_schema,omitempty"`
}

type InternalFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

type InternalToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`  // CC 格式：JSON 字符串
		// 其他协议解析时先填 RawArguments（对象），翻译为 CC 时 Marshal
		RawArguments json.RawMessage `json:"-"`
	} `json:"function"`
}

// ─── 通用请求 ─────────────────────────────────────────────────────────
//
// 端点差异：
//   CC:        POST /v1/chat/completions
//   Responses: POST /v1/responses
//   Anthropic: POST /v1/messages
//   Gemini:    POST /v1/models/{model}:generateContent
//
// System Prompt 位置：
//   CC:        messages[0].role == "system"
//   Responses: 顶层 instructions 字段
//   Anthropic: 顶层 system 字段（注意：必须是数组或字符串，非消息数组元素）
//   Gemini:    顶层 systemInstruction 字段
//
// 关键兼容规则：
//  1. InternalRequest 统一提取 system prompt 到单独字段，不入 messages
//  2. 翻译到 CC 时，把 system 重新插回 messages 开头
//  3. Anthropic 的 system 可以是字符串或数组 → 需要归一化
type InternalRequest struct {
	Model          string                `json:"model"`
	Messages       []InternalMessage     `json:"messages"`      // 不含 system
	SystemPrompt   json.RawMessage       `json:"system_prompt,omitempty"` // 提取后的系统提示
	Tools          []InternalTool        `json:"tools,omitempty"`
	Stream         bool                  `json:"stream,omitempty"`
	Temperature    *float64              `json:"temperature,omitempty"`
	TopP           *float64              `json:"top_p,omitempty"`
	TopK           *int                  `json:"top_k,omitempty"` // Anthropic/Gemini 有，CC 没有
	MaxTokens      int                   `json:"max_tokens,omitempty"`
	MaxOutputTokens int                   `json:"max_output_tokens,omitempty"` // Responses 用此字段名
	StopSequences  []string              `json:"stop,omitempty"`
	ResponseFormat *InternalResponseFormat `json:"response_format,omitempty"`
	UserID         string                `json:"user,omitempty"`
	Seed           *int                  `json:"seed,omitempty"`
	// 原始请求（协议特有，用于回显或调试）
	RawRequest json.RawMessage `json:"-"`
	// 原始协议名（用于选择翻译器）
	Protocol string `json:"-"`
}

// ResponseFormat 格式差异：
//   CC:        {type:"text"} | {type:"json_object"}
//   Responses: {type:"text"} | {type:"json_object"} | {type:"json_schema", json_schema:{...}}
//   Anthropic: 无原生 response_format → 需用 tool 模拟，或用新版 response_format 字段
//   Gemini:    generationConfig.responseMimeType: "text/plain" | "application/json"
type InternalResponseFormat struct {
	Type     string                 `json:"type"` // "text" | "json_object" | "json_schema"
	JsonSchema map[string]interface{} `json:"json_schema,omitempty"`
}

// ─── 通用响应 ─────────────────────────────────────────────────────────

type InternalResponse struct {
	ID       string            `json:"id"`
	Model    string            `json:"model"`
	Choices  []InternalChoice  `json:"choices"`
	Usage    *InternalUsage    `json:"usage,omitempty"`
	Created  int64             `json:"created,omitempty"`
	Object   string            `json:"object,omitempty"` // CC 用 "chat.completion"
}

type InternalChoice struct {
	Index        int             `json:"index"`
	Message      InternalMessage `json:"message"`
	FinishReason string          `json:"finish_reason"` // "stop" | "tool_calls" | "length" | "content_filter"
}

// Usage 字段差异：
//   CC:        prompt_tokens, completion_tokens, total_tokens
//   Responses: input_tokens, output_tokens, total_tokens (+ cache_creation/read)
//   Anthropic: input_tokens, output_tokens, total_tokens
//   Gemini:    prompt_token_count, candidates_token_count, total_token_count
//
// 统一策略：用通用字段名，翻译器做字段名映射
type InternalUsage struct {
	PromptTokens      int `json:"prompt_tokens"`
	CompletionTokens  int `json:"completion_tokens"`
	TotalTokens       int `json:"total_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"` // Responses 特有
	CacheReadTokens    int `json:"cache_read_tokens,omitempty"`     // Responses 特有
}

// ─── Streaming ──────────────────────────────────────────────────────
//
// SSE 事件差异（最复杂的兼容点）：
//
// CC:
//   event: 无 type 字段，纯 data
//   data: {"id":"...","choices":[{"index":0,"delta":{"role":"assistant","content":"..."}}]}
//   data: {"choices":[{"index":0,"delta":{"content":"..."}}]}  // 后续 chunk 不含 role
//   data: {"choices":[{"index":0,"finish_reason":"stop"}]}     // 结束
//   data: [DONE]                                               // 连接结束
//
// Responses:
//   event: response.created
//   event: response.output_delta     ← delta 嵌套在 output_delta.content[0].text
//   event: response.content_block_delta
//   event: response.output_delta     ← 内含 tool_calls
//   event: response.completed
//   注意：Responses 使用 named events，每行格式 "event: <type>\ndata: <json>"
//
// Anthropic:
//   {"type":"message_start","message":{...}}
//   {"type":"content_block_start","content_block":{"type":"text","text":""}}
//   {"type":"content_block_delta","delta":{"type":"text_delta","text":"..."}}
//   {"type":"content_block_stop"}
//   {"type":"message_delta","delta":{"stop_reason":"stop"}}
//   {"type":"message_stop"}
//   注意：Anthropic 用 type 字段区分事件，不是 SSE event 名称
//
// Gemini:
//   {"candidates":[{"content":{"parts":[{"text":"..."}]}}]}
//   注意：Gemini 流式是标准 SSE，但每行 data 是数组或单对象，无 delta 字段
//
// 关键兼容规则：
//  1. 统一输出为 CC 格式的 SSE，用户无感知
//  2. 各协议 delta 字段路径不同，需在 translator 中按路径提取
//  3. Anthropic 的 content_block_start/stop 事件可丢弃或透传
//  4. [DONE] 是 CC 约定，其他协议没有 → 网关层统一追加
type InternalStreamChunk struct {
	ID      string             `json:"id"`
	Model   string             `json:"model"`
	Created int64              `json:"created,omitempty"`
	Choices []InternalChoice   `json:"choices"`
	Usage   *InternalUsage     `json:"usage,omitempty"`
}

// InternalStreamEvent 流式事件，用于内部传递
type InternalStreamEvent struct {
	Type  string               `json:"type"` // "start" | "delta" | "tool_call" | "done" | "error" | "usage"
	Data  *InternalStreamChunk `json:"data,omitempty"`
	Error *StreamError         `json:"error,omitempty"`
}

// StreamError
type StreamError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    int    `json:"code"`
}

// ─── Provider Info ───────────────────────────────────────────────────

type ProviderInfo struct {
	Name       string
	BaseURL    string
	APIToken   string
	Models     []string        // 支持模型列表（用于路由匹配）
	Weight     int             // 负载均衡权重
	TimeoutSec int             // 请求超时秒数
	RateLimit  int             // QPS 限制
	Status     string          // "healthy" | "degraded" | "down"
	Version    string          // Provider 类型 (openai/anthropic/gemini/responses)
	APIVersion string          // API 版本号 (如 anthropic 2023-06-01)
	Capabilities []string      // 上游支持的协议列表 ["openai","anthropic","gemini","responses"]
}

// ─── Monitor ─────────────────────────────────────────────────────────

type RequestRecord struct {
	Time        time.Time
	Method      string
	Path        string
	Model       string
	Protocol    string    // 源协议
	Provider    string    // 目标 provider
	StatusCode  int
	LatencyMs   int64
	RequestBody []byte
	ErrorMsg    string
}

// ProviderMetrics 按秒聚合
type ProviderMetrics struct {
	Second        int64
	Provider      string
	Model         string
	RequestCount  int
	ErrorCount    int
	SuccessCount  int
	LatencyP50    float64
	LatencyP95    float64
	LatencyP99    float64
	LatencySum    float64
	ActiveConns   int
}

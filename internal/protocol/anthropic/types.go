package anthropic

import "encoding/json"

// ═══════════════════════════════════════════════════════════════════════════════
//  Anthropic Messages API 协议类型
//
//  兼容性关键差异（见 schema/internal.go 注释）：
//  - System prompt 分离到顶层 system 字段，不可在 messages 数组中
//  - Tool 定义用 input_schema 而非 parameters
//  - Tool 调用混在 content blocks 中，非独立 tool_calls 字段
//  - 流式事件用 type 字段区分，非 SSE event 名称
//  - 额外 token 限制：max_tokens 最小值 1024（非 1）
// ═══════════════════════════════════════════════════════════════════════════════

// MessageRequest Anthropic Messages API 请求
type MessageRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	System         json.RawMessage `json:"system,omitempty"` // string | []SystemBlock
	Tools          []Tool          `json:"tools,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	TopK           *int            `json:"top_k,omitempty"`
	MaxTokens      int             `json:"max_tokens"`
	StopSequences  []string        `json:"stop_sequences,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	Metadata       *Metadata       `json:"metadata,omitempty"`
	OutputConfig   *OutputConfig   `json:"output_config,omitempty"` // SenseNova 扩展: 推理力度控制
}

type SystemBlock struct {
	Type         string        `json:"type"` // "text"
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type CacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string | []ContentBlock
}

// ContentBlock Anthropic content block（注意与 schema 中不同）
type ContentBlock struct {
	Type         string          `json:"type"` // "text" | "image" | "tool_use" | "tool_result" | "thinking" | "signature"
	Text         string          `json:"text,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`  // thinking 推理文本
	Signature    string          `json:"signature,omitempty"` // thinking 签名
	Source       *ImageSource    `json:"source,omitempty"`
	ID           string          `json:"id,omitempty"`    // tool_use
	Name         string          `json:"name,omitempty"`  // tool_use
	Input        json.RawMessage `json:"input,omitempty"` // tool_use input (JSON object!)
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"` // tool_result content
	CacheControl *CacheControl   `json:"cache_control,omitempty"`
	Citations    []interface{}   `json:"citations"` // 空数组，符合 Anthropic 规范
}

type ImageSource struct {
	Type string `json:"type"` // "base64" | "url"
	// base64 源
	Data      string `json:"data,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	// url 源
	URL string `json:"url,omitempty"`
}

type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

type ResponseFormat struct {
	Type string `json:"type"` // "text" | "json"
}

type Metadata struct {
	UserID string `json:"user_id,omitempty"`
}

// OutputConfig SenseNova 扩展: 推理力度控制
type OutputConfig struct {
	Effort string `json:"effort"` // "low" | "medium" | "high"
}

// MessageResponse Anthropic 响应
type MessageResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"` // "message"
	Role         string         `json:"role"` // "assistant"
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   string         `json:"stop_reason"` // "end_turn" | "max_tokens" | "stop_sequence" | "tool_use"
	StopSequence *string        `json:"stop_sequence"`
	Usage        *Usage         `json:"usage"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"` // Anthropic 2024-11 新增
}

// StreamEvent Anthropic 流式事件（每行一个 JSON 对象，type 字段区分）
type StreamEvent struct {
	Type         string        `json:"type"`
	Message      *EventMessage `json:"message,omitempty"`
	ContentBlock *ContentBlock `json:"content_block,omitempty"`
	Delta        *Delta        `json:"delta,omitempty"`
	MessageDelta *MessageDelta `json:"message_delta,omitempty"`
	Usage        *Usage        `json:"usage,omitempty"` // message_delta 顶层 usage.output_tokens
	Index        int           `json:"index,omitempty"`
}

type EventMessage struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   string         `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        *Usage         `json:"usage"`
}

type Delta struct {
	Type        string `json:"type"` // "text_delta" | "input_json_delta" | "thinking_delta" | "signature_delta"
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`  // thinking_delta 推理文本
	Signature   string `json:"signature,omitempty"` // signature_delta 签名
	PartialJSON string `json:"partial_json,omitempty"`
}

type MessageDelta struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

// AnthropicError
type AnthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	// 其他字段
}

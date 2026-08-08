package chatcompletion

import (
	"encoding/json"
)

// ChatCompletionRequest OpenAI Chat Completions 上游请求
type ChatCompletionRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Tools          []Tool          `json:"tools,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Stop           json.RawMessage `json:"stop,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	User           string          `json:"user,omitempty"`
	Seed           *int            `json:"seed,omitempty"`
	ToolsChoice    json.RawMessage `json:"tool_choice,omitempty"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    Content    `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// Content 支持字符串或 content block 数组，通过 json.RawMessage 保留原始 JSON
type Content struct {
	raw json.RawMessage
}

// Raw 返回 Content 的原始 JSON
func (c Content) Raw() json.RawMessage { return c.raw }

// MarshalJSON 输出保留的原始 JSON
func (c Content) MarshalJSON() ([]byte, error) {
	if c.raw != nil {
		return c.raw, nil
	}
	return nil, nil
}

// UnmarshalJSON 多态解析 content（保留原始 JSON）
func (c *Content) UnmarshalJSON(data []byte) error {
	c.raw = data
	// 先尝试 string
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		return nil
	}
	// 再尝试 []ContentBlock（通用 content block 格式）
	var blocks []json.RawMessage
	if err := json.Unmarshal(data, &blocks); err == nil {
		return nil
	}
	return &json.UnmarshalTypeError{}
}

type Tool struct {
	Type     string       `json:"type"` // "function"
	Function *FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type ResponseFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string                 `json:"name"`
		Schema map[string]interface{} `json:"schema"`
	} `json:"json_schema,omitempty"`
}

// ChatCompletionResponse OpenAI Chat Completions 上游响应
type ChatCompletionResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	Choices           []Choice `json:"choices"`
	Usage             *Usage   `json:"usage,omitempty"`
	SystemFingerprint *string  `json:"system_fingerprint,omitempty"`
}

type Choice struct {
	Index        int       `json:"index"`
	Message      CCMessage `json:"message"`
	FinishReason string    `json:"finish_reason"`
}

type CCMessage struct {
	Role       string     `json:"role"`
	Content    Content    `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatCompletionStreamChunk 流式块
type ChatCompletionStreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
}

type StreamChoice struct {
	Index        int             `json:"index"`
	Delta        StreamDelta     `json:"delta"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
	FinishReason string          `json:"finish_reason,omitempty"`
}

type StreamDelta struct {
	Role       string     `json:"role,omitempty"`
	Content    string     `json:"content,omitempty"`
	Reasoning  string     `json:"reasoning,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// CCError OpenAI 标准错误
type CCError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code"`
}

type ErrorResponse struct {
	Error *CCError `json:"error"`
}

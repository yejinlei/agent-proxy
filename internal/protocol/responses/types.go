package responses

// OpenAI Responses API 协议类型

// ResponseRequest Responses API 请求
type ResponseRequest struct {
	Model           string        `json:"model"`
	Input           []InputItem   `json:"input"`
	Tools           []Tool        `json:"tools,omitempty"`
	Stream          bool          `json:"stream,omitempty"`
	Temperature     *float64      `json:"temperature,omitempty"`
	TopP            *float64      `json:"top_p,omitempty"`
	MaxOutputTokens int           `json:"max_output_tokens,omitempty"`
	StopSequences   []string      `json:"stop_sequences,omitempty"`
	ResponseFormat  *ResponseFormat `json:"response_format,omitempty"`
	Metadata        *Metadata     `json:"metadata,omitempty"`
	Instructions    string        `json:"instructions,omitempty"` // 系统提示
}

type InputItem struct {
	Type      string        `json:"type"`      // "message"
	Role      string        `json:"role"`
	Content   interface{}   `json:"content"`   // string | []ContentBlock
	ToolCalls []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
	Name      string        `json:"name,omitempty"`
}

type Tool struct {
	Type       string                 `json:"type"` // "function"
	Name       string                 `json:"name"`
	Parameters map[string]interface{} `json:"parameters,omitempty"`
}

type ToolCall struct {
	ID    string                 `json:"id"`
	Type  string                 `json:"type"`
	Name  string                 `json:"name"`
	Input map[string]interface{} `json:"input"`
}

type ResponseFormat struct {
	Type string `json:"type"`
}

type Metadata struct {
	UserID string `json:"user_id,omitempty"`
	Seed   *int   `json:"seed,omitempty"`
}

// Response Responses API 响应
type Response struct {
	ID         string     `json:"id"`
	Object     string     `json:"object"`   // "response"
	Status     string     `json:"status"`   // "completed"
	Model      string     `json:"model"`
	Output     []OutputItem `json:"output"`
	Usage      *Usage     `json:"usage,omitempty"`
	StopReason string     `json:"stop_reason,omitempty"`
}

type OutputItem struct {
	Type     string       `json:"type"` // "message"
	ID       string       `json:"id"`
	Role     string       `json:"role"`
	Content  []ContentBlock `json:"content"`
	ToolCalls []ToolCall    `json:"tool_calls,omitempty"`
}

type ContentBlock struct {
	Type      string                 `json:"type"`  // "output_text" | "refusal" | "tool_call" | "tool_result" | "input_text"
	Text      string                 `json:"text"`
	ID        string                 `json:"id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Input     map[string]interface{} `json:"input,omitempty"`
	ToolUseID string                 `json:"tool_call_id,omitempty"` // tool_result 引用
}

type Usage struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	TotalTokens       int `json:"total_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadTokens   int `json:"cache_read_input_tokens,omitempty"`
}

// StreamEvent Responses API 流式事件
type StreamEvent struct {
	Type string     `json:"type"`
	Data *EventData `json:"data"`
}

type EventData struct {
	Type        string     `json:"type"`
	ID          string     `json:"id,omitempty"`
	Event       string     `json:"event,omitempty"`
	OutputIndex int        `json:"output_index"`
	OutputDelta *OutputDelta `json:"output_delta,omitempty"`
	Delta       *Delta     `json:"delta,omitempty"`
	Usage       *Usage     `json:"usage"`
}

type OutputDelta struct {
	Type      string        `json:"type"`
	Role      string        `json:"role"`
	Content   []ContentDelta `json:"content"`
	ToolCalls []ToolCallDelta `json:"tool_calls,omitempty"`
}

type ContentDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ToolCallDelta struct {
	ID    string                 `json:"id"`
	Type  string                 `json:"type"`
	Name  string                 `json:"name"`
	Input map[string]interface{} `json:"input"`
}

type Delta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

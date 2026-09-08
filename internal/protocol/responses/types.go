package responses

import "encoding/json"

// OpenAI Responses API 协议类型

// ResponseRequest Responses API 请求
type ResponseRequest struct {
	Model           string          `json:"model"`
	Input           Input           `json:"input"`
	Tools           []Tool          `json:"tools,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	StopSequences   []string        `json:"stop_sequences,omitempty"`
	ResponseFormat  *ResponseFormat `json:"response_format,omitempty"`
	Metadata        *Metadata       `json:"metadata,omitempty"`
	Instructions    string          `json:"instructions,omitempty"` // 系统提示
}

// Input 兼容 Responses API 两种 input 形式：纯字符串（单消息）或 []InputItem 数组
type Input interface{}

// InputToItems 把 Input 统一转为 []InputItem
func InputToItems(i Input) []InputItem {
	switch v := i.(type) {
	case string:
		return []InputItem{{Type: "message", Role: "user", Content: v}}
	case []InputItem:
		return v
	}
	return nil
}

func (r *ResponseRequest) UnmarshalJSON(data []byte) error {
	type alias ResponseRequest
	aux := &struct {
		Input json.RawMessage `json:"input"`
		*alias
	}{alias: (*alias)(r)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if aux.Input != nil {
		var items []InputItem
		if err := json.Unmarshal(aux.Input, &items); err == nil {
			r.Input = items
		} else {
			var s string
			if err2 := json.Unmarshal(aux.Input, &s); err2 == nil {
				r.Input = s
			}
		}
	}
	return nil
}

type InputItem struct {
	Type       string      `json:"type"` // "message" | "function_call" | "function_call_output" | "reasoning"
	Role       string      `json:"role"`
	Content    interface{} `json:"content"` // string | []ContentBlock
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	Name       string      `json:"name,omitempty"`

	// @AI_GUARD: RESPONSES_INPUT_ITEM_TYPES - 非 message item 的专属字段，缺失会静默丢上下文
	// @CONSTRAINT: Codex 会话历史里 assistant 的工具调用是 type:"function_call"
	//   （name/call_id/arguments，arguments 为 JSON 字符串），工具执行结果紧随其后是
	//   type:"function_call_output"（call_id/output）。这两个字段必须与 item 顶层字段并存，
	//   否则入站侧只能靠 Content 里的 tool_calls/tool_result 兜底，客户端用标准 item 时上下文全丢。
	// @REASON: v0.2.119 — 原先整个非 "message" item 被 inputToMessages 丢弃（26→18 条），
	//   Codex 工具调用结果回灌历史时模型看不到真实输出，只能顺着幻觉编内容。
	CallID    string          `json:"call_id,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    interface{}     `json:"output,omitempty"`
	RawFields map[string]any  `json:"-"` // 解析后的原始 item 字段，供日志统计未知 item 类型
}

// UnmarshalJSON 先按 map 解析原始字段（供日志统计），再按强类型字段解析。
func (i *InputItem) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	i.RawFields = raw
	type alias InputItem
	return json.Unmarshal(data, (*alias)(i))
}

type Tool struct {
	Type       string                 `json:"type"` // "function"
	Name       string                 `json:"name"`
	Parameters map[string]interface{} `json:"parameters,omitempty"`
}

// UnmarshalJSON 兼容两种 tools 格式：
// 1. Responses 原生格式: {"type":"function","name":"foo","parameters":{...}}
// 2. CC 嵌套格式 (Codex 发送): {"type":"function","function":{"name":"foo","parameters":{...}}}
// @REASON: Codex 对 /v1/responses 发送 CC 格式的 tools，function.name 嵌套在 function 字段内
func (t *Tool) UnmarshalJSON(data []byte) error {
	// 先尝试 Responses 原生格式
	type rawTool Tool
	if err := json.Unmarshal(data, (*rawTool)(t)); err == nil && t.Name != "" {
		return nil
	}

	// 回退：尝试 CC 嵌套格式 {type:"function", function:{name, parameters}}
	type ccTool struct {
		Type     string            `json:"type"`
		Function *InternalFunction `json:"function"`
	}
	aux := struct {
		*ccTool
		Parameters map[string]interface{} `json:"parameters,omitempty"`
	}{ccTool: (*ccTool)(new(ccTool))}
	if err := json.Unmarshal(data, &aux); err != nil {
		return json.Unmarshal(data, (*rawTool)(t))
	}
	if aux.Function != nil {
		t.Type = aux.Type
		t.Name = aux.Function.Name
		t.Parameters = aux.Function.Parameters
	} else {
		t.Type = aux.Type
		t.Parameters = aux.Parameters
	}
	return nil
}

type InternalFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
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
	ID         string       `json:"id"`
	Object     string       `json:"object"` // "response"
	Status     string       `json:"status"` // "completed"
	Model      string       `json:"model"`
	Output     []OutputItem `json:"output"`
	Usage      *Usage       `json:"usage,omitempty"`
	StopReason string       `json:"stop_reason,omitempty"`
}

type OutputItem struct {
	Type      string         `json:"type"` // "message"
	ID        string         `json:"id"`
	Role      string         `json:"role"`
	Content   []ContentBlock `json:"content"`
	ToolCalls []ToolCall     `json:"tool_calls,omitempty"`
}

type ContentBlock struct {
	Type      string                 `json:"type"` // "output_text" | "refusal" | "tool_call" | "tool_result" | "input_text" | "input_image"
	Text      string                 `json:"text"`
	ID        string                 `json:"id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Input     map[string]interface{} `json:"input,omitempty"`
	Source    map[string]interface{} `json:"source,omitempty"`
	ToolUseID string                 `json:"tool_call_id,omitempty"` // tool_result 引用
}

type Usage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	TotalTokens         int `json:"total_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// StreamEvent Responses API 流式事件
type StreamEvent struct {
	Type string     `json:"type"`
	Data *EventData `json:"data"`
}

type EventData struct {
	Type        string       `json:"type"`
	ID          string       `json:"id,omitempty"`
	Event       string       `json:"event,omitempty"`
	OutputIndex int          `json:"output_index"`
	OutputDelta *OutputDelta `json:"output_delta,omitempty"`
	Delta       *Delta       `json:"delta,omitempty"`
	Usage       *Usage       `json:"usage"`
	Status      string       `json:"status,omitempty"`      // response.completed 事件中的 status
	StopReason  string       `json:"stop_reason,omitempty"` // response.completed 事件中的 stop_reason
}

type OutputDelta struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   []ContentDelta  `json:"content"`
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

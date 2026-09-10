package responses

import "encoding/json"

// OpenAI Responses API 协议类型

// ResponseRequest Responses API 请求
type ResponseRequest struct {
	Model              string            `json:"model"`
	Input              Input             `json:"input"`
	Tools              []json.RawMessage `json:"tools,omitempty"` // RawMessage：custom 等类型可原样回写
	Stream             bool              `json:"stream,omitempty"`
	Temperature        *float64          `json:"temperature,omitempty"`
	TopP               *float64          `json:"top_p,omitempty"`
	MaxOutputTokens    int               `json:"max_output_tokens,omitempty"`
	StopSequences      []string          `json:"stop_sequences,omitempty"`
	ResponseFormat     *ResponseFormat   `json:"response_format,omitempty"`
	Metadata           *Metadata         `json:"metadata,omitempty"`
	Instructions       string            `json:"instructions,omitempty"` // 系统提示
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
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
	CallID    string         `json:"call_id,omitempty"`
	Arguments string         `json:"arguments,omitempty"`
	Output    interface{}    `json:"output,omitempty"`
	RawFields map[string]any `json:"-"` // 解析后的原始 item 字段，供日志统计未知 item 类型
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

// @AI_GUARD: RESPONSES_TOOL_RAW_PASSTHROUGH - 工具元素必须是可原样透传的 JSON
// @CONSTRAINT: 入站 type 不只 function（还有 custom / web_search / tool_search /
//   image_generation / namespace，且客户端会随版本新增）。ResponseRequest.Tools 的元素
//   不能是强类型 []Tool，否则出站只能重建成 function、丢掉其余类型。用 []json.RawMessage
//   保留入站原始字节，出站按目标协议能力决定原样回写还是丢弃。
// @RELATED: translator.go toolsToResponses（Raw 原样回写）
// @REASON: v0.2.131 之前 []Tool 强类型 + Type 硬编码 "function" 是「非 function 工具全丢」
//   的出站侧根因；入站侧根因见 translator.go TranslateRequest 的 @AI_GUARD。

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
	Type   string                 `json:"type"` // "output_text" | "refusal" | "tool_call" | "tool_result" | "input_text" | "input_image"
	Text   string                 `json:"text"`
	ID     string                 `json:"id,omitempty"`
	Name   string                 `json:"name,omitempty"`
	Input  map[string]interface{} `json:"input,omitempty"`
	Source map[string]interface{} `json:"source,omitempty"`
	// ImageURL 是 Responses 协议 input_image 块的顶层字段（OpenAI 官方格式，也是
	// Codex CLI 线上格式）。与 Anthropic 不同：Anthropic 图片数据放在
	// source:{type:"base64",data,media_type} 里，Responses 放在顶层 image_url 里。
	// v0.2.126 及之前本字段不存在，出站只写 source:{}，OpenAI 系上游读不到图片。
	ImageURL  string `json:"image_url,omitempty"`
	ToolUseID string `json:"tool_call_id,omitempty"` // tool_result 引用
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

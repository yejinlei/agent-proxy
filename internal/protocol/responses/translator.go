package responses

import (
	"context"
	"encoding/json"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// ═══════════════════════════════════════════════════════════════════════════════
//  Responses 协议翻译器
//
//  兼容性关键差异：
//
//  1. 端点: POST /v1/responses（非 /v1/chat/completions）
//
//  2. 请求体结构完全不同：
//     - CC: {messages: [...], model: "...", tools: [...]}
//     - Responses: {input: [InputItem], model: "...", tools: [...]}
//     - ⚠️ input 是数组，每个元素是 {type:"message", role, content}
//     - ⚠️ system prompt 在顶层 instructions 字段，非 messages 元素
//
//  3. TOOL 定义：
//     - CC: tools: [{type:"function", function:{name, description, parameters}}]
//     - Responses: tools: [{type:"function", name, parameters}]
//     - ⚠️ Responses 无 description 字段（可能忽略或放在 name 中）
//
//  4. TOOL CALL（响应解析）：
//     - CC: tool_calls: [{id, type:"function", function:{name, arguments:"{json}"}}]
//     - Responses: tool_calls: [{id, type:"function", name, input:{...}}]
//     - ⚠️ input 是 JSON 对象（非字符串）→ 需 Marshal 为 arguments 字符串
//     - ⚠️ Responses 有独立 tool_calls 字段（与 CC 类似）
//
//  5. 响应结构：
//     - CC: {choices: [{message: {role, content, tool_calls}}]}
//     - Responses: {output: [{type:"message", content: [{type:"output_text", text: "..."}]}]}
//     - ⚠️ 输出用 output 数组，每个 output_item 有独立的 content blocks
//
//  6. USAGE 字段：
//     - input_tokens → prompt_tokens
//     - output_tokens → completion_tokens
//     - total_tokens 一致
//     - ⚠️ 额外有 cache_creation_input_tokens / cache_read_input_tokens
//
//  7. STOP_REASON：
//     - stop → stop
//     - max_output_tokens → length
//     - tool_calls → tool_calls
//     - other → stop
//
//  8. STREAMING（最重要！）：
//     - 使用 named SSE events: "event: response.created\n" + "data: {...}\n"
//     - 事件类型: response.created, response.output_delta, response.content_block_delta,
//               response.completed, response.failed
//     - ⚠️ output_delta.content[0].text 是实际文本增量（非顶层 content）
//     - ⚠️ delta.content_block_delta.delta.text 也是文本增量（冗余，可任选其一）
//     - ⚠️ 输出用 output_index 标识多个输出
// ═══════════════════════════════════════════════════════════════════════════════

type ResponsesTranslator struct{}

func NewResponsesTranslator() *ResponsesTranslator {
	return &ResponsesTranslator{}
}

func (t *ResponsesTranslator) Protocol() string { return "responses" }

// ═══════════════════════════════════════════════════════════════════════════════
//  REQUEST: Responses ResponseRequest → InternalRequest (入站解析)
// ═══════════════════════════════════════════════════════════════════════════════

func (t *ResponsesTranslator) TranslateRequest(rawReq json.RawMessage) (*schema.InternalRequest, error) {
	var req ResponseRequest
	if err := json.Unmarshal(rawReq, &req); err != nil {
		return nil, err
	}

	// --- 1. Instructions → SystemPrompt ---
	var systemContent json.RawMessage
	if req.Instructions != "" {
		systemContent = json.RawMessage(`"` + req.Instructions + `"`)
	}

	// --- 2. Input → Messages ---
	messages := inputToMessages(req.Input)

	// --- 3. Tools → InternalTools ---
	var tools []schema.InternalTool
	for _, tool := range req.Tools {
		tools = append(tools, schema.InternalTool{
			Type: "function",
			Function: &schema.InternalFunction{
				Name:       tool.Name,
				Parameters: tool.Parameters,
			},
		})
	}

	// --- 4. Metadata ---
	var userID string
	var seed *int
	if req.Metadata != nil {
		userID = req.Metadata.UserID
		seed = req.Metadata.Seed
	}

	return &schema.InternalRequest{
		Model:         req.Model,
		Messages:      messages,
		SystemPrompt:  systemContent,
		Tools:         tools,
		Stream:        req.Stream,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		MaxTokens:     req.MaxOutputTokens,
		StopSequences: req.StopSequences,
		UserID:        userID,
		Seed:          seed,
		RawRequest:    rawReq,
		Protocol:      "responses",
	}, nil
}

func inputToMessages(items []InputItem) []schema.InternalMessage {
	var msgs []schema.InternalMessage
	for _, item := range items {
		if item.Type != "message" {
			continue
		}

		msg := schema.InternalMessage{Role: schema.Role(item.Role)}

		// 提取 text
		var text string
		switch c := item.Content.(type) {
		case string:
			text = c
		case []interface{}:
			// content blocks
			var textParts []string
			for _, cb := range c {
				block, ok := cb.(map[string]interface{})
				if !ok {
					continue
				}
				if block["type"] == "output_text" || block["type"] == "input_text" {
					if t, ok := block["text"].(string); ok {
						textParts = append(textParts, t)
					}
				}
			}
			text = joinText(textParts)
		}
		msg.Content = json.RawMessage(`"` + text + `"`)

		// Tool calls
		for _, tc := range item.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Input)
			msg.ToolCalls = append(msg.ToolCalls, schema.InternalToolCall{
				ID:   tc.ID,
				Type: tc.Type,
				Function: struct {
					Name         string          `json:"name"`
					Arguments    string          `json:"arguments"`
					RawArguments json.RawMessage `json:"-"`
				}{
					Name:         tc.Name,
					Arguments:    string(argsJSON),
					RawArguments: argsJSON,
				},
			})
		}

		msgs = append(msgs, msg)
	}
	return msgs
}

// ═══════════════════════════════════════════════════════════════════════════════
//  RESPONSE: InternalResponse → Responses Response (出站)
// ═══════════════════════════════════════════════════════════════════════════════

func (t *ResponsesTranslator) TranslateResponse(resp *schema.InternalResponse) (json.RawMessage, error) {
	var contentBlocks []ContentBlock

	for _, choice := range resp.Choices {
		var text string
		if choice.Message.Content != nil {
			json.Unmarshal(choice.Message.Content, &text)
		}
		if text != "" {
			contentBlocks = append(contentBlocks, ContentBlock{Type: "output_text", Text: text})
		}

		for _, tc := range choice.Message.ToolCalls {
			contentBlocks = append(contentBlocks, ContentBlock{
				Type: "tool_call",
				ID:   tc.ID,
				Name: tc.Function.Name,
				Input: func() map[string]interface{} {
					m := make(map[string]interface{})
					json.Unmarshal(tc.Function.RawArguments, &m)
					return m
				}(),
			})
		}
	}

	var usage *Usage
	if resp.Usage != nil {
		usage = &Usage{
			InputTokens:         resp.Usage.PromptTokens,
			OutputTokens:        resp.Usage.CompletionTokens,
			TotalTokens:         resp.Usage.TotalTokens,
			CacheCreationTokens: resp.Usage.CacheCreationTokens,
			CacheReadTokens:     resp.Usage.CacheReadTokens,
		}
	}

	respObj := Response{
		ID:         resp.ID,
		Object:     "response",
		Status:     "completed",
		Model:      resp.Model,
		StopReason: mapStopReasonReverse(resp.Choices[0].FinishReason),
		Output: []OutputItem{
			{
				Type:    "message",
				Role:    "assistant",
				Content: contentBlocks,
			},
		},
		Usage: usage,
	}

	return json.Marshal(respObj)
}

func mapStopReasonReverse(reason string) string {
	switch reason {
	case "stop":
		return "stop"
	case "length":
		return "max_output_tokens"
	case "tool_calls":
		return "tool_calls"
	default:
		return "stop"
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
//  STREAM: InternalStreamEvents → Responses SSE
// ═══════════════════════════════════════════════════════════════════════════════

func (t *ResponsesTranslator) TranslateStream(ctx context.Context, events <-chan schema.InternalStreamEvent, fn func(eventData []byte, isDone bool)) {
	for {
		select {
		case <-ctx.Done():
			fn([]byte("data: [DONE]\n\n"), true)
			return
		case event, ok := <-events:
			if !ok {
				fn([]byte("data: [DONE]\n\n"), true)
				return
			}

			switch event.Type {
			case "error":
				errData := t.TranslateError(event.Error)
				fn(append([]byte("event: response.failed\ndata: "), errData...), false)
				fn([]byte("\n\n"), false)
				continue

			case "start":
				if event.Data != nil && event.Data.ID != "" {
					eventData := EventData{ID: event.Data.ID, Type: "response"}
					raw, _ := json.Marshal(StreamEvent{Type: "response.created", Data: &eventData})
					fn(append([]byte("event: response.created\ndata: "), raw...), false)
					fn([]byte("\n\n"), false)
				}
				continue

			case "delta":
				if event.Data != nil && len(event.Data.Choices) > 0 {
					choice := event.Data.Choices[0]
					eventData := EventData{OutputIndex: choice.Index}

					if choice.Message.Content != nil {
						var text string
						json.Unmarshal(choice.Message.Content, &text)
						if text != "" {
							eventData.OutputDelta = &OutputDelta{
								Type:    "message",
								Role:    "assistant",
								Content: []ContentDelta{{Type: "delta", Text: text}},
							}
						}
					}

					for _, tc := range choice.Message.ToolCalls {
						eventData.OutputDelta = &OutputDelta{
							Type: "message",
							ToolCalls: []ToolCallDelta{{
								ID:   tc.ID,
								Type: "function",
								Name: tc.Function.Name,
								Input: func() map[string]interface{} {
									m := make(map[string]interface{})
									json.Unmarshal(tc.Function.RawArguments, &m)
									return m
								}(),
							}},
						}
					}

					if eventData.OutputDelta != nil {
						raw, _ := json.Marshal(StreamEvent{Type: "response.output_delta", Data: &eventData})
						fn(append([]byte("event: response.output_delta\ndata: "), raw...), false)
						fn([]byte("\n\n"), false)
					}
				}
				continue

			case "done":
				eventData := EventData{Type: "response"}
				if event.Data != nil && event.Data.Usage != nil {
					eventData.Usage = &Usage{
						InputTokens:  event.Data.Usage.PromptTokens,
						OutputTokens: event.Data.Usage.CompletionTokens,
						TotalTokens:  event.Data.Usage.TotalTokens,
					}
				}
				raw, _ := json.Marshal(StreamEvent{Type: "response.completed", Data: &eventData})
				fn(append([]byte("event: response.completed\ndata: "), raw...), false)
				fn([]byte("\n\n"), false)
				fn([]byte("data: [DONE]\n\n"), true)
				return
			}
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
//  REQUEST: InternalRequest → Responses ResponseRequest
// ═══════════════════════════════════════════════════════════════════════════════

func (t *ResponsesTranslator) TranslateToProvider(req *schema.InternalRequest) (*ResponseRequest, error) {
	// --- 1. Messages → Input 数组 ---
	input := buildInputArray(req.Messages)

	// --- 2. System prompt → instructions ---
	instructions := ""
	if len(req.SystemPrompt) > 0 {
		var text string
		json.Unmarshal(req.SystemPrompt, &text)
		instructions = text
	}

	// --- 3. Tools ---
	tools := toolsToResponses(req.Tools)

	return &ResponseRequest{
		Model:           req.Model,
		Input:           input,
		Tools:           tools,
		Stream:          req.Stream,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		MaxOutputTokens: req.MaxOutputTokens,
		StopSequences:   req.StopSequences,
		ResponseFormat:  responseFormatToResponses(req.ResponseFormat),
		Metadata:        &Metadata{UserID: req.UserID, Seed: req.Seed},
		Instructions:    instructions,
	}, nil
}

func buildInputArray(msgs []schema.InternalMessage) []InputItem {
	var items []InputItem

	for _, msg := range msgs {
		if msg.Role == schema.RoleSystem {
			continue // system 已提取到 instructions
		}

		// ⚠️ RoleTool: 在 Responses 入站中必须构造为 tool_result 内容块
		if msg.Role == schema.RoleTool {
			var contentText string
			if msg.Content != nil {
				json.Unmarshal(msg.Content, &contentText)
			}
			// tool_result 的 content 是嵌套的 content block 数组
			toolResultContent, _ := json.Marshal([]ContentBlock{
				{Type: "input_text", Text: contentText},
			})
			items = append(items, InputItem{
				Type: "message",
				Role: "user",
				Content: json.RawMessage(toolResultContent),
				ToolCalls: []ToolCall{
					{Type: "function", ID: msg.ToolCallID, Name: msg.Name},
				},
			})
			continue
		}

		item := InputItem{
			Type: "message",
			Role: string(msg.Role),
		}

		// 文本内容
		var text string
		if msg.Content != nil {
			json.Unmarshal(msg.Content, &text)
		}

		// 如果有 tool_calls，需要转换为 Responses tool_call blocks
		if len(msg.ToolCalls) > 0 {
			contentBlocks := []ContentBlock{
				{Type: "input_text", Text: text},
			}
			for _, tc := range msg.ToolCalls {
				// ⚠️ input 是 JSON 对象
				var inputMap map[string]interface{}
				if tc.Function.RawArguments != nil {
					json.Unmarshal(tc.Function.RawArguments, &inputMap)
				}
				contentBlocks = append(contentBlocks, ContentBlock{
					Type:  "tool_call",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: inputMap,
				})
			}
			item.Content = contentBlocks
		} else {
			// 简单文本（请求侧用 input_text）
			item.Content = ContentBlock{Type: "input_text", Text: text}
		}

		items = append(items, item)
	}

	return items
}

func toolsToResponses(tools []schema.InternalTool) []Tool {
	var result []Tool
	for _, tool := range tools {
		if tool.Function == nil {
			continue
		}
		result = append(result, Tool{
			Type: "function",
			Name: tool.Function.Name,
			// ⚠️ Responses 无 description 字段
			Parameters: tool.Function.Parameters,
		})
	}
	return result
}

func responseFormatToResponses(rf *schema.InternalResponseFormat) *ResponseFormat {
	if rf == nil {
		return nil
	}
	return &ResponseFormat{Type: rf.Type}
}

// ═══════════════════════════════════════════════════════════════════════════════
//  RESPONSE: Responses Response → InternalResponse
// ═══════════════════════════════════════════════════════════════════════════════

func (t *ResponsesTranslator) TranslateFromProvider(raw json.RawMessage) (*schema.InternalResponse, error) {
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}

	var choiceMessage schema.InternalMessage
	choiceMessage.Role = schema.RoleAssistant

	var textParts []string
	var toolCalls []schema.InternalToolCall

	for _, item := range resp.Output {
		if item.Type != "message" {
			continue
		}
		for _, block := range item.Content {
			switch block.Type {
			case "output_text":
				textParts = append(textParts, block.Text)
			case "tool_call":
				// ⚠️ input 是对象，需 Marshal 为字符串
				argsJSON, _ := json.Marshal(block.Input)
				toolCalls = append(toolCalls, schema.InternalToolCall{
					ID:   block.ID,
					Type: "function",
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{
						Name:         block.Name,
						Arguments:    string(argsJSON),
						RawArguments: argsJSON,
					},
				})
			}
		}
	}

	if len(toolCalls) > 0 {
		choiceMessage.ToolCalls = toolCalls
	}

	choiceMessage.Content = json.RawMessage(`"` + joinText(textParts) + `"`)

	var usage *schema.InternalUsage
	if resp.Usage != nil {
		usage = &schema.InternalUsage{
			PromptTokens:        resp.Usage.InputTokens,
			CompletionTokens:    resp.Usage.OutputTokens,
			TotalTokens:         resp.Usage.TotalTokens,
			CacheCreationTokens: resp.Usage.CacheCreationTokens,
			CacheReadTokens:     resp.Usage.CacheReadTokens,
		}
	}

	return &schema.InternalResponse{
		ID:    resp.ID,
		Model: resp.Model,
		Choices: []schema.InternalChoice{
			{
				Index:        0,
				Message:      choiceMessage,
				FinishReason: mapStopReason(resp.StopReason),
			},
		},
		Usage:  usage,
		Object: "chat.completion",
	}, nil
}

func mapStopReason(reason string) string {
	switch reason {
	case "stop":
		return "stop"
	case "max_output_tokens":
		return "length"
	case "tool_calls":
		return "tool_calls"
	case "max_duration":
		return "length"
	default:
		return "stop"
	}
}

func joinText(parts []string) string {
	result := ""
	for _, p := range parts {
		// 输出块内容可能是 JSON 字符串，解引号
		text := p
		if len(text) >= 2 && text[0] == '"' && text[len(text)-1] == '"' {
			text = text[1 : len(text)-1]
		}
		result += text
	}
	return result
}

// ═══════════════════════════════════════════════════════════════════════════════
//  STREAMING: Responses 流式事件 → InternalStreamEvent
// ═══════════════════════════════════════════════════════════════════════════════

func (t *ResponsesTranslator) TranslateStreamEvent(event *StreamEvent) *schema.InternalStreamEvent {
	if event.Data == nil {
		return nil
	}

	data := event.Data

	switch event.Type {
	case "response.output_delta":
		return t.translateOutputDelta(data)

	case "response.content_block_delta":
		// ⚠️ content_block_delta.delta.text 也是文本增量（与 output_delta 冗余）
		if data.Delta != nil && data.Delta.Text != "" {
			return &schema.InternalStreamEvent{
				Type: "delta",
				Data: &schema.InternalStreamChunk{
					Choices: []schema.InternalChoice{
						{
							Index: data.OutputIndex,
							Message: schema.InternalMessage{
								Role:    schema.RoleAssistant,
								Content: json.RawMessage(`"` + data.Delta.Text + `"`),
							},
						},
					},
				},
			}
		}
		return nil

	case "response.created":
		return &schema.InternalStreamEvent{
			Type: "start",
			Data: &schema.InternalStreamChunk{
				ID: data.ID,
			},
		}

	case "response.completed":
		var usage *schema.InternalUsage
		if data.Usage != nil {
			usage = &schema.InternalUsage{
				PromptTokens:     data.Usage.InputTokens,
				CompletionTokens: data.Usage.OutputTokens,
				TotalTokens:      data.Usage.TotalTokens,
			}
		}
		return &schema.InternalStreamEvent{
			Type: "done",
			Data: &schema.InternalStreamChunk{
				Choices: []schema.InternalChoice{{FinishReason: "stop"}},
				Usage:   usage,
			},
		}

	case "response.failed":
		return &schema.InternalStreamEvent{
			Type: "error",
			Error: &schema.StreamError{
				Message: "response failed",
				Type:    "server_error",
				Code:    500,
			},
		}

	default:
		// 忽略未知事件
		return nil
	}
}

func (t *ResponsesTranslator) translateOutputDelta(data *EventData) *schema.InternalStreamEvent {
	if data.OutputDelta == nil || len(data.OutputDelta.Content) == 0 {
		return nil
	}

	var deltaText string
	for _, cb := range data.OutputDelta.Content {
		if cb.Type == "delta" {
			deltaText = cb.Text
		}
	}

	if deltaText == "" {
		// 没有文本 delta，检查是否有 tool_call delta
		for _, tc := range data.OutputDelta.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Input)
			toolCall := schema.InternalToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: struct {
					Name         string          `json:"name"`
					Arguments    string          `json:"arguments"`
					RawArguments json.RawMessage `json:"-"`
				}{
					Name:         tc.Name,
					Arguments:    string(argsJSON),
					RawArguments: argsJSON,
				},
			}
			return &schema.InternalStreamEvent{
				Type: "delta",
				Data: &schema.InternalStreamChunk{
					Choices: []schema.InternalChoice{
						{
							Index: data.OutputIndex,
							Message: schema.InternalMessage{
								ToolCalls: []schema.InternalToolCall{toolCall},
							},
						},
					},
				},
			}
		}
		return nil
	}

	return &schema.InternalStreamEvent{
		Type: "delta",
		Data: &schema.InternalStreamChunk{
			Choices: []schema.InternalChoice{
				{
					Index: data.OutputIndex,
					Message: schema.InternalMessage{
						Role:    schema.RoleAssistant,
						Content: json.RawMessage(`"` + deltaText + `"`),
					},
				},
			},
		},
	}
}

// TranslateStreamToCCSSE 将 Responses 流式输出翻译为 CC 格式 SSE
func (t *ResponsesTranslator) TranslateStreamToCCSSE(ctx context.Context, events <-chan *StreamEvent, fn func(data []byte, isDone bool)) {
	for {
		select {
		case <-ctx.Done():
			fn([]byte("data: [DONE]\n\n"), true)
			return
		case event, ok := <-events:
			if !ok {
				fn([]byte("data: [DONE]\n\n"), true)
				return
			}

			internalEvent := t.TranslateStreamEvent(event)
			if internalEvent == nil {
				continue
			}

			if internalEvent.Type == "done" {
				fn([]byte("data: [DONE]\n\n"), true)
				return
			}

			if internalEvent.Type == "error" {
				errData, _ := json.Marshal(internalEvent.Error)
				fn(append([]byte("data: {\"error\":"), errData...), false)
				fn([]byte("}\n\n"), false)
				continue
			}

			data := ToCCStreamChunk(internalEvent.Data)
			fn(append([]byte("data: "), data...), false)
			fn([]byte("\n\n"), false)
		}
	}
}

func (t *ResponsesTranslator) TranslateError(err *schema.StreamError) json.RawMessage {
	errData, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"message": err.Message,
			"type":    "invalid_request_error",
			"code":    err.Type,
		},
	})
	return errData
}

// ToCCStreamChunk 构建 CC 格式流式块
func ToCCStreamChunk(chunk *schema.InternalStreamChunk) json.RawMessage {
	choices := make([]map[string]interface{}, len(chunk.Choices))

	for i, choice := range chunk.Choices {
		if choice.FinishReason != "" {
			choices[i] = map[string]interface{}{
				"index":         choice.Index,
				"finish_reason": choice.FinishReason,
			}
			continue
		}

		delta := map[string]interface{}{}
		if choice.Message.Role != "" {
			delta["role"] = string(choice.Message.Role)
		}
		if choice.Message.Content != nil {
			var text string
			json.Unmarshal(choice.Message.Content, &text)
			delta["content"] = text
		}
		if len(choice.Message.ToolCalls) > 0 {
			delta["tool_calls"] = choice.Message.ToolCalls
		}

		choices[i] = map[string]interface{}{
			"index": choice.Index,
			"delta": delta,
		}
	}

	raw, _ := json.Marshal(map[string]interface{}{
		"id":      chunk.ID,
		"object":  "chat.completion.chunk",
		"model":   chunk.Model,
		"choices": choices,
	})
	return raw
}

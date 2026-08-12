package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// AnthropicTranslator Anthropic Messages API 翻译器
// 兼容性详见文件头部注释（1-8 号差异点）
type AnthropicTranslator struct {
	APIVersion string
}

func NewAnthropicTranslator(version string) *AnthropicTranslator {
	return &AnthropicTranslator{APIVersion: version}
}

func (t *AnthropicTranslator) Protocol() string { return "anthropic" }

// ═══════════════════════════════════════════════════════════════════════════════
//  REQUEST: Anthropic MessageRequest → InternalRequest (入站解析)
// ═══════════════════════════════════════════════════════════════════════════════

// TranslateRequest 将 Anthropic 原生请求解析为 InternalRequest
func (t *AnthropicTranslator) TranslateRequest(ctx context.Context, rawReq json.RawMessage) (*schema.InternalRequest, error) {
	var antReq MessageRequest
	if err := json.Unmarshal(rawReq, &antReq); err != nil {
		return nil, err
	}

	// --- 1. System → SystemPrompt ---
	var systemContent json.RawMessage
	if len(antReq.System) > 0 {
		// 透传原始 JSON（string 或 []SystemBlock）
		systemContent = antReq.System
	}

	// --- 2. Messages → InternalMessages ---
	var messages []schema.InternalMessage
	for _, msg := range antReq.Messages {
		switch msg.Role {
		case "assistant":
			msg, err := messageToInternal(msg)
			if err != nil {
				return nil, err
			}
			messages = append(messages, msg)
		case "user":
			msgs, err := messagesToInternal(msg)
			if err != nil {
				return nil, err
			}
			messages = append(messages, msgs...)
		case "system":
			// 部分 Anthropic 版本允许 system 在 messages 数组中
			if len(systemContent) == 0 {
				systemContent = msg.Content
			}
		}
	}

	// --- 3. Tools → InternalTools ---
	var tools []schema.InternalTool
	for _, tool := range antReq.Tools {
		tools = append(tools, schema.InternalTool{
			Type: "function",
			Function: &schema.InternalFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
			},
		})
	}

	// --- 4. MaxTokens（最小 1024） ---
	maxTokens := antReq.MaxTokens
	if maxTokens > 0 && maxTokens < 1024 {
		maxTokens = 1024
	}

	return &schema.InternalRequest{
		Model:         antReq.Model,
		Messages:      messages,
		SystemPrompt:  systemContent,
		Tools:         tools,
		Stream:        antReq.Stream,
		Temperature:   antReq.Temperature,
		TopP:          antReq.TopP,
		TopK:          antReq.TopK,
		MaxTokens:     maxTokens,
		StopSequences: antReq.StopSequences,
		UserID:        userIDFromMetadata(antReq.Metadata),
		RawRequest:    rawReq,
		Protocol:      "anthropic",
	}, nil
}

// messageToInternal 将 Anthropic assistant message → InternalMessage
func messageToInternal(msg Message) (schema.InternalMessage, error) {
	var im schema.InternalMessage
	im.Role = schema.RoleAssistant

	var blocks []ContentBlock
	if len(msg.Content) > 0 {
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			return im, err
		}
	}

	var textParts []string
	var toolCalls []schema.InternalToolCall

	for _, block := range blocks {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "tool_use":
			toolCalls = append(toolCalls, schema.InternalToolCall{
				ID:   block.ID,
				Type: "function",
				Function: struct {
					Name         string          `json:"name"`
					Arguments    string          `json:"arguments"`
					RawArguments json.RawMessage `json:"-"`
				}{
					Name:         block.Name,
					Arguments:    string(block.Input),
					RawArguments: block.Input,
				},
			})
		}
	}

	im.Content, _ = json.Marshal(joinText(textParts))
	if len(toolCalls) > 0 {
		im.ToolCalls = toolCalls
	}
	return im, nil
}

// messagesToInternal 将 Anthropic user message 解析（含 tool_result）
func messagesToInternal(msg Message) ([]schema.InternalMessage, error) {
	var result []schema.InternalMessage

	var blocks []ContentBlock
	if len(msg.Content) > 0 {
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			// 回退：纯字符串
			var text string
			if err := json.Unmarshal(msg.Content, &text); err == nil {
				return []schema.InternalMessage{
					{Role: schema.RoleUser, Content: json.RawMessage(msg.Content)},
				}, nil
			}
			return nil, err
		}
	}

	if len(blocks) == 0 {
		return nil, nil
	}

	// 混合 blocks：text 合并到一个 user 消息，tool_result 单独
	var textParts []string
	var userMsg *schema.InternalMessage
	var contentBlocks []schema.InternalContentBlock

	for _, block := range blocks {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
			cb := schema.InternalContentBlock{Type: "text", Text: block.Text}
			contentBlocks = append(contentBlocks, cb)
			if userMsg == nil {
				userMsg = &schema.InternalMessage{Role: schema.RoleUser}
			}
		case "thinking":
			// 透传 thinking 推理内容块（DeepSeek 等模型的推理过程，通常出现在 assistant 消息中）
			cb := schema.InternalContentBlock{Type: "thinking", Thinking: block.Thinking, Signature: block.Signature}
			contentBlocks = append(contentBlocks, cb)
		case "image":
			cb := schema.InternalContentBlock{
				Type:      "image",
				Data:      "",
				MediaType: "",
			}
			if block.Source != nil {
				cb.Data = block.Source.Data
				cb.MediaType = block.Source.MediaType
			}
			contentBlocks = append(contentBlocks, cb)
			if userMsg == nil {
				userMsg = &schema.InternalMessage{Role: schema.RoleUser}
			}
		case "tool_result":
			// tool_result 归为 tool 角色
			var contentText string
			if len(block.Content) > 0 {
				contentText = string(block.Content)
				// 去掉外层 JSON 数组包装，取第一个 text
				var arr []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(block.Content, &arr) == nil && len(arr) > 0 && arr[0].Type == "text" {
					contentText = arr[0].Text
				}
			}
			result = append(result, schema.InternalMessage{
				Role:       schema.RoleTool,
				Content:    func() json.RawMessage { b, _ := json.Marshal(contentText); return b }(),
				ToolCallID: block.ToolUseID,
			})
		}
	}

	if userMsg != nil {
		userMsg.Content, _ = json.Marshal(strings.Join(textParts, "\n"))
		if len(contentBlocks) > 0 {
			userMsg.ContentBlocks = contentBlocks
		}
		result = append(result, *userMsg)
	}

	return result, nil
}

func userIDFromMetadata(m *Metadata) string {
	if m != nil {
		return m.UserID
	}
	return ""
}

// ═══════════════════════════════════════════════════════════════════════════════
//  RESPONSE: InternalResponse → Anthropic MessageResponse (出站)
// ═══════════════════════════════════════════════════════════════════════════════

// TranslateResponse 将 InternalResponse 翻译为 Anthropic 原生响应
func (t *AnthropicTranslator) TranslateResponse(resp *schema.InternalResponse) (json.RawMessage, error) {
	var contentBlocks []ContentBlock

	for _, choice := range resp.Choices {
		// 优先使用 ContentBlocks（含图片），回退到 Content 字符串
		if len(choice.Message.ContentBlocks) > 0 {
			for _, cb := range choice.Message.ContentBlocks {
				switch cb.Type {
				case "text":
					if cb.Text != "" {
						contentBlocks = append(contentBlocks, ContentBlock{Type: "text", Text: cb.Text})
					}
				case "thinking":
					// 透传 thinking 推理内容块（DeepSeek 等模型的推理过程）
					if cb.Thinking != "" {
						contentBlocks = append(contentBlocks, ContentBlock{Type: "thinking", Thinking: cb.Thinking, Signature: cb.Signature})
					}
				case "image":
					if cb.Data != "" {
						contentBlocks = append(contentBlocks, ContentBlock{
							Type: "image",
							Source: &ImageSource{
								Type:      "base64",
								Data:      cb.Data,
								MediaType: cb.MediaType,
							},
						})
					} else if cb.URL != "" {
						contentBlocks = append(contentBlocks, ContentBlock{
							Type: "image",
							Source: &ImageSource{
								Type:      "url",
								URL:       cb.URL,
								MediaType: cb.MediaType,
							},
						})
					}
				}
			}
		} else {
			var text string
			if choice.Message.Content != nil {
				json.Unmarshal(choice.Message.Content, &text)
			}
			if text != "" {
				contentBlocks = append(contentBlocks, ContentBlock{Type: "text", Text: text})
			}
		}

		for _, tc := range choice.Message.ToolCalls {
			contentBlocks = append(contentBlocks, ContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: tc.Function.RawArguments,
			})
		}
	}

	stopReason := mapStopReasonReverse(resp.Choices[0].FinishReason)

	usage := (*Usage)(nil)
	if resp.Usage != nil {
		usage = &Usage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
			TotalTokens:  resp.Usage.TotalTokens,
		}
	}

	antResp := MessageResponse{
		ID:         resp.ID,
		Type:       "message",
		Role:       "assistant",
		Content:    contentBlocks,
		Model:      resp.Model,
		StopReason: stopReason,
		Usage:      usage,
	}
	return json.Marshal(antResp)
}

func mapStopReasonReverse(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
//  STREAM: InternalStreamEvents → Anthropic SSE
// ═══════════════════════════════════════════════════════════════════════════════

// TranslateStream 将内部流式事件翻译为 Anthropic 格式 SSE
func (t *AnthropicTranslator) TranslateStream(ctx context.Context, events <-chan schema.InternalStreamEvent, fn func(eventData []byte, isDone bool)) {
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
				fn(append([]byte("event: error\ndata: "), errData...), false)
				fn([]byte("\n\n"), false)
				continue

			case "start":
				if event.Data != nil {
					msg := EventMessage{ID: event.Data.ID, Type: "message", Role: "assistant"}
					if event.Data.Model != "" {
						msg.Model = event.Data.Model
					}
					raw, _ := json.Marshal(StreamEvent{Type: "message_start", Message: &msg})
					fn(append([]byte("data: "), raw...), false)
					fn([]byte("\n\n"), false)
				}
				continue

			case "delta":
				if event.Data != nil && len(event.Data.Choices) > 0 {
					choice := event.Data.Choices[0]
					var delta *Delta
					if choice.Message.Content != nil {
						var text string
						json.Unmarshal(choice.Message.Content, &text)
						delta = &Delta{Type: "text_delta", Text: text}
					}
					if len(choice.Message.ToolCalls) > 0 {
						tc := choice.Message.ToolCalls[0]
						delta = &Delta{Type: "input_json_delta", PartialJSON: tc.Function.Arguments}
					}
					if delta != nil {
						raw, _ := json.Marshal(StreamEvent{
							Type:  "content_block_delta",
							Index: choice.Index,
							Delta: delta,
						})
						fn(append([]byte("data: "), raw...), false)
						fn([]byte("\n\n"), false)
					}
				}
				continue

			case "done":
				stopReason := "end_turn"
				if event.Data != nil && len(event.Data.Choices) > 0 {
					stopReason = mapStopReasonReverse(event.Data.Choices[0].FinishReason)
				}
				var usage *Usage
				if event.Data != nil && event.Data.Usage != nil {
					usage = &Usage{
						InputTokens:  event.Data.Usage.PromptTokens,
						OutputTokens: event.Data.Usage.CompletionTokens,
						TotalTokens:  event.Data.Usage.TotalTokens,
					}
				}
				raw, _ := json.Marshal(StreamEvent{
					Type:         "message_delta",
					MessageDelta: &MessageDelta{StopReason: stopReason},
				})
				fn(append([]byte("data: "), raw...), false)
				fn([]byte("\n\n"), false)

				// 最后发送 usage 行
				if usage != nil {
					usageRaw, _ := json.Marshal(map[string]interface{}{
						"type":  "message_stop",
						"usage": usage,
					})
					fn(append([]byte("data: "), usageRaw...), false)
					fn([]byte("\n\n"), false)
				}

				fn([]byte("data: [DONE]\n\n"), true)
				return
			}
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
//  REQUEST: InternalRequest → Anthropic MessageRequest
// ═══════════════════════════════════════════════════════════════════════════════

func (t *AnthropicTranslator) TranslateToProvider(req *schema.InternalRequest) (*MessageRequest, error) {
	system, err := extractAnthropicSystem(req.SystemPrompt)
	if err != nil {
		return nil, fmt.Errorf("extract system prompt: %w", err)
	}

	messages, err := messagesToAnthropic(req.Messages)
	if err != nil {
		return nil, fmt.Errorf("translate messages: %w", err)
	}

	tools := toolsToAnthropic(req.Tools)

	return &MessageRequest{
		Model:         req.Model,
		Messages:      messages,
		System:        system,
		Tools:         tools,
		Stream:        req.Stream,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		TopK:          req.TopK,
		MaxTokens:     req.MaxTokens,
		StopSequences: req.StopSequences,
		Metadata: &Metadata{
			UserID: req.UserID,
		},
	}, nil
}

func extractAnthropicSystem(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return json.Marshal(s)
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return raw, nil
	}

	systemBlocks := make([]SystemBlock, len(blocks))
	for i, b := range blocks {
		systemBlocks[i] = SystemBlock{
			Type: "text",
			Text: b.Text,
		}
	}
	return json.Marshal(systemBlocks)
}

func messagesToAnthropic(msgs []schema.InternalMessage) ([]Message, error) {
	var result []Message

	for _, msg := range msgs {
		switch msg.Role {
		case schema.RoleAssistant:
			content, err := assistantContentToAnthropic(msg)
			if err != nil {
				return nil, err
			}
			result = append(result, Message{
				Role:    "assistant",
				Content: content,
			})

		case schema.RoleTool:
			// ⚠️ Anthropic 将 tool 结果归为 user 消息；结果文本从 msg.Content 读取
			var contentText string
			if msg.Content != nil {
				json.Unmarshal(msg.Content, &contentText)
			}
			toolContent, _ := json.Marshal([]ContentBlock{
				{
					Type:      "tool_result",
					ToolUseID: msg.ToolCallID,
					Content: func() json.RawMessage {
						b, _ := json.Marshal([]ContentBlock{{Type: "text", Text: contentText}})
						return b
					}(),
				},
			})
			result = append(result, Message{
				Role:    "user",
				Content: toolContent,
			})

		case schema.RoleUser:
			content := buildAnthropicUserContent(msg)
			result = append(result, Message{
				Role:    "user",
				Content: content,
			})
		}
	}

	return result, nil
}

func assistantContentToAnthropic(msg schema.InternalMessage) (json.RawMessage, error) {
	var blocks []ContentBlock

	var text string
	if msg.Content != nil {
		json.Unmarshal(msg.Content, &text)
	}
	if text != "" {
		blocks = append(blocks, ContentBlock{Type: "text", Text: text})
	}

	for _, tc := range msg.ToolCalls {
		blocks = append(blocks, ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: tc.Function.RawArguments,
		})
	}

	return json.Marshal(blocks)
}

// buildAnthropicUserContent 将 InternalMessage 转换为用户消息的 content
// 优先使用 ContentBlocks（含图片），回退到 Content 纯文本
func buildAnthropicUserContent(msg schema.InternalMessage) json.RawMessage {
	if len(msg.ContentBlocks) > 0 {
		var blocks []ContentBlock
		for _, cb := range msg.ContentBlocks {
			switch cb.Type {
			case "text":
				blocks = append(blocks, ContentBlock{Type: "text", Text: cb.Text})
			case "image":
				if cb.Data != "" {
					blocks = append(blocks, ContentBlock{
						Type: "image",
						Source: &ImageSource{
							Type:      "base64",
							Data:      cb.Data,
							MediaType: cb.MediaType,
						},
					})
				} else if cb.URL != "" {
					blocks = append(blocks, ContentBlock{
						Type: "image",
						Source: &ImageSource{
							Type:      "url",
							URL:       cb.URL,
							MediaType: cb.MediaType,
						},
					})
				}
			}
		}
		if len(blocks) > 0 {
			data, _ := json.Marshal(blocks)
			return data
		}
	}

	// 回退: 纯字符串
	var text string
	if msg.Content != nil {
		json.Unmarshal(msg.Content, &text)
	}
	return func() json.RawMessage { b, _ := json.Marshal(text); return b }()
}

func toolsToAnthropic(tools []schema.InternalTool) []Tool {
	var result []Tool
	for _, tool := range tools {
		if tool.Function == nil {
			continue
		}
		inputSchema := tool.Function.Parameters
		if inputSchema == nil {
			inputSchema = map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			}
		}
		result = append(result, Tool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			InputSchema: inputSchema,
		})
	}
	return result
}

// ═══════════════════════════════════════════════════════════════════════════════
//  RESPONSE: Anthropic Response → InternalResponse
// ═══════════════════════════════════════════════════════════════════════════════

func (t *AnthropicTranslator) TranslateFromProvider(raw json.RawMessage) (*schema.InternalResponse, error) {
	var resp MessageResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}

	var choiceMessage schema.InternalMessage
	choiceMessage.Role = schema.RoleAssistant

	var textParts []string
	var toolCalls []schema.InternalToolCall
	var contentBlocks []schema.InternalContentBlock

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
			contentBlocks = append(contentBlocks, schema.InternalContentBlock{
				Type: "text",
				Text: block.Text,
			})
		case "image":
			cb := schema.InternalContentBlock{Type: "image"}
			if block.Source != nil {
				cb.Data = block.Source.Data
				cb.MediaType = block.Source.MediaType
				cb.URL = block.Source.URL
			}
			contentBlocks = append(contentBlocks, cb)
		case "tool_use":
			// ⚠️ tool_use 混在 content blocks 中
			toolCalls = append(toolCalls, schema.InternalToolCall{
				ID:   block.ID,
				Type: "function",
				Function: struct {
					Name         string          `json:"name"`
					Arguments    string          `json:"arguments"`
					RawArguments json.RawMessage `json:"-"`
				}{
					Name:         block.Name,
					Arguments:    string(block.Input),
					RawArguments: block.Input,
				},
			})
		}
	}

	if len(toolCalls) > 0 {
		choiceMessage.ToolCalls = toolCalls
	}

	choiceMessage.Content, _ = json.Marshal(joinText(textParts))
	if len(contentBlocks) > 0 {
		choiceMessage.ContentBlocks = contentBlocks
	}

	var usage *schema.InternalUsage
	if resp.Usage != nil {
		usage = &schema.InternalUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.TotalTokens,
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
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

func joinText(parts []string) string {
	result := ""
	for _, p := range parts {
		result += p
	}
	return result
}

// ═══════════════════════════════════════════════════════════════════════════════
//  STREAMING
// ═══════════════════════════════════════════════════════════════════════════════

func (t *AnthropicTranslator) TranslateStreamEvent(raw json.RawMessage) *schema.InternalStreamEvent {
	var event StreamEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return &schema.InternalStreamEvent{
			Type: "error",
			Error: &schema.StreamError{
				Message: "parse error: " + err.Error(),
				Code:    400,
			},
		}
	}

	switch event.Type {
	case "content_block_delta":
		deltaText := ""
		if event.Delta != nil {
			switch event.Delta.Type {
			case "text_delta":
				deltaText = event.Delta.Text
			case "thinking_delta":
				deltaText = event.Delta.Thinking
			case "signature_delta":
				// signature_delta 是验证签名，不包含文本内容，忽略
				return nil
			default:
				deltaText = event.Delta.Text
			}
		}
		return &schema.InternalStreamEvent{
			Type: "delta",
			Data: &schema.InternalStreamChunk{
				Choices: []schema.InternalChoice{
					{
						Index: event.Index,
						Message: schema.InternalMessage{
							Role:    schema.RoleAssistant,
							Content: func() json.RawMessage { b, _ := json.Marshal(deltaText); return b }(),
						},
					},
				},
			},
		}

	case "message_delta":
		// 提取 usage（Anthropic message_delta 事件顶层包含 usage.output_tokens）
		var usage *schema.InternalUsage
		if event.Usage != nil {
			usage = &schema.InternalUsage{
				CompletionTokens: event.Usage.OutputTokens,
				TotalTokens:      event.Usage.TotalTokens,
			}
		}
		return &schema.InternalStreamEvent{
			Type: "done",
			Data: &schema.InternalStreamChunk{
				Choices: []schema.InternalChoice{
					{
						Index:        0,
						FinishReason: mapStopReason(event.MessageDelta.StopReason),
					},
				},
				Usage: usage,
			},
		}

	case "message_start":
		model := ""
		role := schema.RoleAssistant
		if event.Message != nil {
			model = event.Message.Model
			if event.Message.Role != "" {
				role = schema.Role(event.Message.Role)
			}
		}
		return &schema.InternalStreamEvent{
			Type: "start",
			Data: &schema.InternalStreamChunk{
				ID:    event.Message.ID,
				Model: model,
				Choices: []schema.InternalChoice{
					{
						Index:   0,
						Message: schema.InternalMessage{Role: role},
					},
				},
			},
		}

	default:
		return nil // 忽略中间事件
	}
}

func (t *AnthropicTranslator) TranslateStreamToCCSSE(ctx context.Context, events <-chan json.RawMessage, fn func(data []byte, isDone bool)) {
	for {
		select {
		case <-ctx.Done():
			fn([]byte("data: [DONE]\n\n"), true)
			return
		case raw, ok := <-events:
			if !ok {
				fn([]byte("data: [DONE]\n\n"), true)
				return
			}

			event := t.TranslateStreamEvent(raw)
			if event == nil {
				continue
			}

			if event.Type == "done" {
				// 发送 usage chunk（如果有）再发送 [DONE]
				if event.Data != nil && event.Data.Usage != nil {
					chunk := ToCCStreamChunk(event.Data)
					fn(append([]byte("data: "), chunk...), false)
					fn([]byte("\n\n"), false)
				}
				fn([]byte("data: [DONE]\n\n"), true)
				return
			}

			data := ToCCStreamChunk(event.Data)
			fn(append([]byte("data: "), data...), false)
			fn([]byte("\n\n"), false)
		}
	}
}

func (t *AnthropicTranslator) TranslateError(err *schema.StreamError) json.RawMessage {
	anthErr := AnthropicError{
		Type:    err.Type,
		Message: err.Message,
	}
	data, _ := json.Marshal(anthErr)
	return data
}

// ToCCStreamChunk 复用 responses 包中的通用函数
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
		"usage":   chunk.Usage,
	})
	return raw
}

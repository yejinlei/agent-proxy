package anthropic

import (
	"context"
	"encoding/json"
	"fmt"

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

	// ⚠️ Anthropic 要求 max_tokens 最小值 1024
	maxTokens := req.MaxTokens
	if maxTokens > 0 && maxTokens < 1024 {
		maxTokens = 1024
	}

	return &MessageRequest{
		Model:         req.Model,
		Messages:      messages,
		System:        system,
		Tools:         tools,
		Stream:        req.Stream,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		TopK:          req.TopK,
		MaxTokens:     maxTokens,
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
            // ⚠️ Anthropic 将 tool 结果归为 user 消息
            toolContent, _ := json.Marshal([]ContentBlock{
                {
                    Type: "tool_result",
                    ToolUseID: msg.ToolCallID,
                    Content: json.RawMessage(`[{"type":"text","text":"` + msg.Name + `"}]`),
                },
            })
			result = append(result, Message{
                Role:    "user",
                Content: toolContent,
            })

        case schema.RoleUser:
            // content 用原始字符串
            var text string
            if msg.Content != nil {
                json.Unmarshal(msg.Content, &text)
            }
            result = append(result, Message{
                Role:    "user",
                Content: json.RawMessage(`"` + text + `"`),
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
            Type:     "tool_use",
            ID:       tc.ID,
            Name:     tc.Function.Name,
            Input:    tc.Function.RawArguments,
        })
	}

	return json.Marshal(blocks)
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

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
            textParts = append(textParts, block.Text)
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

	choiceMessage.Content = json.RawMessage(`"` + joinText(textParts) + `"`)

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
            deltaText = event.Delta.Text
        }
        return &schema.InternalStreamEvent{
            Type: "delta",
            Data: &schema.InternalStreamChunk{
                Choices: []schema.InternalChoice{
                    {
                        Index: event.Index,
                        Message: schema.InternalMessage{
                            Role:    schema.RoleAssistant,
                            Content: json.RawMessage(`"` + deltaText + `"`),
                        },
                    },
                },
            },
        }

    case "message_delta":
        return &schema.InternalStreamEvent{
            Type: "done",
            Data: &schema.InternalStreamChunk{
                Choices: []schema.InternalChoice{
                    {
                        Index:        0,
                        FinishReason: mapStopReason(event.MessageDelta.StopReason),
                    },
                },
            },
        }

    case "message_start":
        model := ""
        if event.Message != nil {
            model = event.Message.Model
        }
        return &schema.InternalStreamEvent{
            Type: "start",
            Data: &schema.InternalStreamChunk{
                ID:    event.Message.ID,
                Model: model,
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

// ToCCStreamChunk 将 InternalStreamChunk 构建为 CC 格式流式块
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

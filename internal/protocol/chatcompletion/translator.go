package chatcompletion

import (
	"context"
	"encoding/json"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// ═══════════════════════════════════════════════════════════════════════════════
//  ChatCompletion → InternalRequest 翻译器
//
//  兼容性要点：
//  1. System prompt 在 messages 数组中（role:"system"），需提取到顶层 SystemPrompt
//  2. content 可以是 string 或 []ContentBlock（多模态）→ 保留为 json.RawMessage
//  3. tool_calls.arguments 是 JSON 字符串 → 存入 InternalToolCall.RawArguments
//  4. CC 是网关入口协议，出口方向不需要额外 FromInternal 翻译器
// ═══════════════════════════════════════════════════════════════════════════════

type ChatCompletionTranslator struct{}

func (t *ChatCompletionTranslator) Protocol() string { return "chatcompletion" }

// TranslateRequest 将 ChatCompletionRequest 翻译为 InternalRequest
func (t *ChatCompletionTranslator) TranslateRequest(ctx context.Context, rawReq json.RawMessage) (*schema.InternalRequest, error) {
	var ccReq ChatCompletionRequest
	if err := json.Unmarshal(rawReq, &ccReq); err != nil {
		return nil, err
	}

	// --- 1. System prompt 提取 ---
	var systemContent json.RawMessage
	var messages []schema.InternalMessage

	for _, msg := range ccReq.Messages {
		if msg.Role == "system" {
			systemContent = msg.Content.Raw()
			continue
		}
		messages = append(messages, messageToInternal(msg))
	}

	// --- 2. Tools 翻译 ---
	var tools []schema.InternalTool
	for _, tool := range ccReq.Tools {
		tools = append(tools, schema.InternalTool{
			Type: "function",
			Function: &schema.InternalFunction{
				Name:        tool.Function.Name,
				Description: tool.Function.Description,
				Parameters:  tool.Function.Parameters,
			},
		})
	}

	// --- 3. ResponseFormat ---
	var respFmt *schema.InternalResponseFormat
	if ccReq.ResponseFormat != nil {
		jf := &schema.InternalResponseFormat{Type: ccReq.ResponseFormat.Type}
		if ccReq.ResponseFormat.JSONSchema != nil {
			jf.JsonSchema = ccReq.ResponseFormat.JSONSchema.Schema
		}
		respFmt = jf
	}

	return &schema.InternalRequest{
		Model:          ccReq.Model,
		Messages:       messages,
		SystemPrompt:   systemContent,
		Tools:          tools,
		Stream:         ccReq.Stream,
		Temperature:    ccReq.Temperature,
		TopP:           ccReq.TopP,
		MaxTokens:      ccReq.MaxTokens,
		StopSequences:  parseStopSequences(ccReq.Stop),
		ResponseFormat: respFmt,
		UserID:         ccReq.User,
		Seed:           ccReq.Seed,
		RawRequest:     rawReq,
		Protocol:       "chatcompletion",
	}, nil
}

// messageToInternal 将 CC Message → InternalMessage
func messageToInternal(msg Message) schema.InternalMessage {
	var toolCalls []schema.InternalToolCall
	for _, tc := range msg.ToolCalls {
		toolCalls = append(toolCalls, schema.InternalToolCall{
			ID:   tc.ID,
			Type: tc.Type,
			Function: struct {
				Name         string          `json:"name"`
				Arguments    string          `json:"arguments"`
				RawArguments json.RawMessage `json:"-"`
			}{
				Name:         tc.Function.Name,
				Arguments:    tc.Function.Arguments,
				RawArguments: json.RawMessage(tc.Function.Arguments),
			},
		})
	}

	return schema.InternalMessage{
		Role:       schema.Role(msg.Role),
		Content:    msg.Content.Raw(),
		ToolCalls:  toolCalls,
		ToolCallID: msg.ToolCallID,
		Name:       msg.Name,
	}
}

// parseStopSequences 解析 stop（string 或 []string）
func parseStopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var seqs []string
	if err := json.Unmarshal(raw, &seqs); err == nil {
		return seqs
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}
	}
	return nil
}

// TranslateResponse 将 InternalResponse 翻译为 CC 响应
func (t *ChatCompletionTranslator) TranslateResponse(resp *schema.InternalResponse) (json.RawMessage, error) {
	ccResp := InternalToCCResponse(resp)
	return json.Marshal(ccResp)
}

// TranslateStream 将内部流式事件翻译为 CC SSE（ChatCompletion 是网关入口协议，直接透传）
func (t *ChatCompletionTranslator) TranslateStream(ctx context.Context, events <-chan schema.InternalStreamEvent, fn func(eventData []byte, isDone bool)) {
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
			if event.Type == "error" {
				errObj := map[string]interface{}{
					"error": map[string]interface{}{
						"message": event.Error.Message,
						"type":    "invalid_request_error",
						"code":    event.Error.Code,
					},
				}
				errData, _ := json.Marshal(errObj)
				fn(append([]byte("data: "), append(errData, '\n', '\n')...), false)
				fn([]byte("data: [DONE]\n\n"), true)
				return
			}
			if event.Type == "done" {
				fn([]byte("data: [DONE]\n\n"), true)
				return
			}
			// 将 InternalStreamChunk 转换为 CC 格式 delta chunk
			if event.Data == nil {
				continue
			}
			chunk := buildCCStreamChunk(event.Data)
			chunkData, _ := json.Marshal(chunk)
			fn(append([]byte("data: "), append(chunkData, '\n', '\n')...), false)
		}
	}
}

// buildCCStreamChunk 将 InternalStreamChunk 转为 CC SSE 格式（含 delta 字段）
func buildCCStreamChunk(chunk *schema.InternalStreamChunk) *ChatCompletionStreamChunk {
	choices := make([]StreamChoice, len(chunk.Choices))
	for i, c := range chunk.Choices {
		if c.FinishReason != "" {
			choices[i] = StreamChoice{Index: c.Index, FinishReason: c.FinishReason}
			continue
		}
		delta := StreamDelta{}
		if c.Message.Role != "" {
			delta.Role = string(c.Message.Role)
		}
		if c.Message.Content != nil {
			var text string
			if json.Unmarshal(c.Message.Content, &text) == nil {
				delta.Content = text
			}
		}
		for _, tc := range c.Message.ToolCalls {
			delta.ToolCalls = append(delta.ToolCalls, ToolCall{
				ID:   tc.ID,
				Type: tc.Type,
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
			})
		}
		choices[i] = StreamChoice{Index: c.Index, Delta: delta, FinishReason: c.FinishReason}
	}
	usage := &Usage{}
	if chunk.Usage != nil {
		usage.PromptTokens = chunk.Usage.PromptTokens
		usage.CompletionTokens = chunk.Usage.CompletionTokens
		usage.TotalTokens = chunk.Usage.TotalTokens
	}
	return &ChatCompletionStreamChunk{
		ID:      chunk.ID,
		Object:  "chat.completion.chunk",
		Model:   chunk.Model,
		Created: chunk.Created,
		Choices: choices,
		Usage:   usage,
	}
}

// InternalToCCResponse 将 InternalResponse 转为 CC 响应（包外可见）
func InternalToCCResponse(resp *schema.InternalResponse) *ChatCompletionResponse {
	ccResp := &ChatCompletionResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: resp.Created,
		Model:   resp.Model,
		Choices: make([]Choice, len(resp.Choices)),
	}

	for i, choice := range resp.Choices {
		content := unmarshalJSONString(choice.Message.Content)
		toolCalls := make([]ToolCall, len(choice.Message.ToolCalls))
		for j, tc := range choice.Message.ToolCalls {
			toolCalls[j] = ToolCall{
				ID:   tc.ID,
				Type: tc.Type,
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			}
		}
		ccResp.Choices[i] = Choice{
			Index: choice.Index,
			Message: CCMessage{
				Role:      string(choice.Message.Role),
				Content:   content,
				ToolCalls: toolCalls,
			},
			FinishReason: choice.FinishReason,
		}
	}

	if resp.Usage != nil {
		ccResp.Usage = &Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		}
	}

	return ccResp
}

// TranslateError 将内部错误翻译为 CC 标准错误
func (t *ChatCompletionTranslator) TranslateError(err *schema.StreamError) json.RawMessage {
	ccErr := CCError{
		Message: err.Message,
		Type:    "invalid_request_error",
		Code:    err.Type,
	}
	resp := ErrorResponse{Error: &ccErr}
	data, _ := json.Marshal(resp)
	return data
}

// unmarshalJSONString 将 json.RawMessage 解析为 string
func unmarshalJSONString(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// 非纯字符串（如内容块数组），回退为原始 JSON 字符串，避免静默丢弃
	return string(raw)
}

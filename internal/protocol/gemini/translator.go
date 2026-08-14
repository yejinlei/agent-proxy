package gemini

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// geminiModelPathKey context 键类型：由 gateway 在 /v1/models/{model}:generateContent 路径中
// 解析出的模型名写入 ctx，供 TranslateRequest 和网关层（如透传模式）在 body 无 model 时兜底使用。
type geminiModelPathKey struct{}

// WithGeminiModel 将路径中提取的模型名写入 context，供 TranslateRequest 兜底。
func WithGeminiModel(ctx context.Context, model string) context.Context {
	return context.WithValue(ctx, geminiModelPathKey{}, model)
}

// GeminiModelFromContext 从 context 中读取路径中的模型名（透传模式等场景使用）。
func GeminiModelFromContext(ctx context.Context) (string, bool) {
	m, ok := ctx.Value(geminiModelPathKey{}).(string)
	return m, ok
}

// ═══════════════════════════════════════════════════════════════════════════════
//  Gemini 协议翻译器
//
//  兼容性关键差异：
//
//  1. 端点格式: POST /v1/models/{model}:generateContent（非标准 REST 路径）
//
//  2. ROLE 映射：
//     - user ↔ user
//     - assistant ↔ model（Gemini 用 model 而非 assistant）
//     - system ↔ systemInstruction（顶层字段）
//
//  3. CONTENT 结构：
//     - CC: content 是 string 或 []ContentBlock
//     - Gemini: parts 数组 [{text: "..."}, {inline_data: {...}}, {function_call: ...}]
//
//  4. TOOL 定义：
//     - CC: tools: [{type:"function", function:{name, description, parameters}}]
//     - Gemini: tools: {functionDeclarations:[{name, description, parameters}]}
//     - ⚠️ Gemini 多一层 "tools.functionDeclarations" 包装
//
//  5. TOOL CALL：
//     - CC: tool_calls: [{id, type:"function", function:{name, arguments:"{json}"}}]
//     - Gemini: parts: [{function_call:{name, args:{...}}}]
//     - ⚠️ function_call 在 parts 数组中，不是独立字段
//     - ⚠️ args 是 JSON 对象，不是字符串（需 Marshal）
//     - ⚠️ Gemini 没有 tool_call_id → 网关生成唯一 ID
//
//  6. TOOL RESULT：
//     - CC: role:"tool", tool_call_id, content:"result"
//     - Gemini: role:"user", parts:[{function_response:{name, response:{result}}}]
//     - ⚠️ function_response.response 可以是嵌套对象，CC 的 content 是字符串
//
//  7. USAGE 字段名：
//     - prompt_token_count → prompt_tokens
//     - candidates_token_count → completion_tokens
//     - total_token_count → total_tokens
//
//  8. FINISH_REASON 映射：
//     - STOP → stop
//     - MAX_TOKENS → length
//     - SAFETY → content_filter
//     - RECITATION → content_filter
//
//  9. STREAMING：
//     - Gemini 流式每行是一个完整 GenerateContentResponse 对象
//     - 每行含 candidates[0].content.parts[0].text（非 delta）
//     - 最后一行可能含 usageMetadata
//     - ⚠️ 不是 CC 格式的 delta，需要转换为 delta
//     - ⚠️ [DONE] 是网关追加，Gemini 本身不发送
// ═══════════════════════════════════════════════════════════════════════════════

type GeminiTranslator struct{}

func NewGeminiTranslator() *GeminiTranslator {
	return &GeminiTranslator{}
}

func (t *GeminiTranslator) Protocol() string { return "gemini" }

// ═══════════════════════════════════════════════════════════════════════════════
//  REQUEST: Gemini GenerateContentRequest → InternalRequest (入站解析)
// ═══════════════════════════════════════════════════════════════════════════════

// @AI_GUARD: GEMINI_TRANSLATE_REQUEST - Gemini GenerateContent → InternalRequest（Central Schema 入口）
// @CONSTRAINT: 消息格式转换必须经过 Central Schema，禁止直接翻译到其他协议
//   - systemInstruction → SystemPrompt
//   - contents 逐条转换（role 映射：user/model → user/assistant）
//   - parts 数组转换为 content/text 或 contentBlocks
//   - 新增字段映射时必须同步修改 GenerateContentRequest 结构体
//
// @RELATED: chatcompletion/translator.go TranslateRequest, anthropic/translator.go TranslateRequest
func (t *GeminiTranslator) TranslateRequest(ctx context.Context, rawReq json.RawMessage) (*schema.InternalRequest, error) {
	var req GenerateContentRequest
	if err := json.Unmarshal(rawReq, &req); err != nil {
		return nil, err
	}

	// --- 1. systemInstruction → SystemPrompt ---
	var systemContent json.RawMessage
	if req.SystemInstruction != nil {
		systemContent = json.RawMessage(fmt.Sprintf(`"%s"`, req.SystemInstruction.Parts[0].Text))
	}

	// --- 2. Contents → Messages ---
	messages := contentsToMessages(req.Contents)

	// --- 3. Tools → InternalTools ---
	var tools []schema.InternalTool
	if req.Tools != nil {
		for _, decl := range req.Tools.FunctionDeclarations {
			tools = append(tools, schema.InternalTool{
				Type: "function",
				Function: &schema.InternalFunction{
					Name:        decl.Name,
					Description: decl.Description,
					Parameters:  decl.Parameters,
				},
			})
		}
	}

	// --- 4. GenerationConfig → params ---
	cfg := req.GenerationConfig
	maxTokens := 0
	if cfg != nil {
		maxTokens = cfg.MaxOutputTokens
	}

	// 提取模型名：优先 body 中的 model，其次路径中的模型名（由 gateway 写入 ctx），最后兜底
	model := req.Model
	if model == "" {
		if m, ok := ctx.Value(geminiModelPathKey{}).(string); ok && m != "" {
			model = m
		}
	}
	if model == "" {
		model = "gemini-1.5-flash"
	}

	return &schema.InternalRequest{
		Model:         model,
		Messages:      messages,
		SystemPrompt:  systemContent,
		Tools:         tools,
		Stream:        req.Stream,
		Temperature:   cfgToFloatPtr(req.GenerationConfig, "temperature"),
		TopP:          cfgToFloatPtr(req.GenerationConfig, "top_p"),
		TopK:          cfgToIntPtr(req.GenerationConfig, "top_k"),
		MaxTokens:     maxTokens,
		StopSequences: extractStopSequences(req.GenerationConfig),
		UserID:        "",
		RawRequest:    rawReq,
		Protocol:      "gemini",
	}, nil
}

func contentsToMessages(contents []Content) []schema.InternalMessage {
	var msgs []schema.InternalMessage
	for _, c := range contents {
		msg := schema.InternalMessage{}
		switch c.Role {
		case "user":
			msg.Role = schema.RoleUser
		case "model":
			msg.Role = schema.RoleAssistant
		default:
			msg.Role = schema.RoleUser
		}

		var textParts []string
		var toolCalls []schema.InternalToolCall
		var functionResponses []FunctionResponse
		var contentBlocks []schema.InternalContentBlock

		for _, part := range c.Parts {
			if part.Text != "" {
				textParts = append(textParts, part.Text)
				contentBlocks = append(contentBlocks, schema.InternalContentBlock{
					Type: "text",
					Text: part.Text,
				})
			}
			if part.InlineData != nil {
				contentBlocks = append(contentBlocks, schema.InternalContentBlock{
					Type:      "image",
					Data:      part.InlineData.Data,
					MediaType: part.InlineData.MimeType,
				})
			}
			if part.FunctionCall != nil {
				argsJSON, _ := json.Marshal(part.FunctionCall.Args)
				toolCalls = append(toolCalls, schema.InternalToolCall{
					ID:   fmt.Sprintf("gc_%d", len(toolCalls)),
					Type: "function",
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{
						Name:         part.FunctionCall.Name,
						Arguments:    string(argsJSON),
						RawArguments: part.FunctionCall.Args,
					},
				})
			}
			if part.FunctionResponse != nil {
				functionResponses = append(functionResponses, *part.FunctionResponse)
			}
		}

		msg.Content, _ = json.Marshal(joinText(textParts))
		if len(toolCalls) > 0 {
			msg.ToolCalls = toolCalls
		}
		if len(contentBlocks) > 0 {
			msg.ContentBlocks = contentBlocks
		}
		msgs = append(msgs, msg)

		// tool_result 归入后续 user 消息
		for _, fr := range functionResponses {
			var resultText string
			if v, ok := fr.Response["result"]; ok {
				resultText, _ = v.(string)
			}
			msgs = append(msgs, schema.InternalMessage{
				Role:       schema.RoleTool,
				Content:    func() json.RawMessage { b, _ := json.Marshal(resultText); return b }(),
				ToolCallID: fr.Name,
				Name:       resultText,
			})
		}
	}
	return msgs
}

func cfgToFloatPtr(cfg *GenerationConfig, field string) *float64 {
	if cfg == nil {
		return nil
	}
	// 字段名从配置中获取
	switch field {
	case "temperature":
		if cfg.Temperature != nil {
			return cfg.Temperature
		}
	case "top_p":
		if cfg.TopP != nil {
			return cfg.TopP
		}
	}
	return nil
}

func cfgToIntPtr(cfg *GenerationConfig, field string) *int {
	if cfg == nil || field != "top_k" || cfg.TopK == nil {
		return nil
	}
	return cfg.TopK
}

func extractStopSequences(cfg *GenerationConfig) []string {
	if cfg != nil {
		return cfg.StopSequences
	}
	return nil
}

// ═══════════════════════════════════════════════════════════════════════════════
//  RESPONSE: InternalResponse → Gemini GenerateContentResponse (出站)
// ═══════════════════════════════════════════════════════════════════════════════

// @AI_GUARD: GEMINI_TRANSLATE_RESPONSE - InternalResponse → Gemini GenerateContentResponse（Central Schema 出口）
// @CONSTRAINT: 必须正确映射 InternalResponse 到 Gemini 原生响应格式
//   - ContentBlocks 转换为 candidates[].content.parts[]
//   - 文本块 → text part，工具调用 → functionCall part
//
// @RELATED: chatcompletion/translator.go TranslateResponse, anthropic/translator.go TranslateResponse
func (t *GeminiTranslator) TranslateResponse(resp *schema.InternalResponse) (json.RawMessage, error) {
	var parts []Part
	var toolCalls []schema.InternalToolCall

	for _, choice := range resp.Choices {
		if len(choice.Message.ContentBlocks) > 0 {
			for _, cb := range choice.Message.ContentBlocks {
				switch cb.Type {
				case "text":
					if cb.Text != "" {
						parts = append(parts, Part{Text: cb.Text})
					}
				case "image":
					if cb.Data != "" {
						parts = append(parts, Part{
							InlineData: &InlineData{
								MimeType: cb.MediaType,
								Data:     cb.Data,
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
				parts = append(parts, Part{Text: text})
			}
		}
		toolCalls = choice.Message.ToolCalls
	}

	for _, tc := range toolCalls {
		parts = append(parts, Part{
			FunctionCall: &FunctionCall{
				Name: tc.Function.Name,
				Args: tc.Function.RawArguments,
			},
		})
	}

	var usage *UsageMetadata
	if resp.Usage != nil {
		usage = &UsageMetadata{
			PromptTokenCount:     resp.Usage.PromptTokens,
			CandidatesTokenCount: resp.Usage.CompletionTokens,
			TotalTokenCount:      resp.Usage.TotalTokens,
		}
	}

	content := &Content{Role: "model", Parts: parts}
	if len(parts) == 0 {
		content.Parts = []Part{{Text: ""}}
	}

	finishReason := mapFinishReasonReverse(resp.Choices[0].FinishReason)

	gemResp := GenerateContentResponse{
		Candidates: []Candidate{
			{
				Index:        resp.Choices[0].Index,
				Content:      content,
				FinishReason: finishReason,
			},
		},
		UsageMetadata: usage,
	}

	return json.Marshal(gemResp)
}

func mapFinishReasonReverse(reason string) string {
	switch reason {
	case "stop":
		return "STOP"
	case "length":
		return "MAX_TOKENS"
	case "tool_calls":
		return "STOP"
	case "content_filter":
		return "SAFETY"
	default:
		return "STOP"
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
//  STREAM: InternalStreamEvents → Gemini SSE
// ═══════════════════════════════════════════════════════════════════════════════

// @AI_GUARD: GEMINI_TRANSLATE_STREAM - InternalStreamEvent → Gemini SSE（流式出口）
// @CONSTRAINT: Gemini SSE 格式为 "data: <json>\n\n"，结束标记为 "data: [DONE]\n\n"
//   - ctx.Done() 时发送 [DONE] 结束
//
// @RELATED: chatcompletion/translator.go TranslateStream (格式相同), anthropic/translator.go TranslateStream (格式不同)
func (t *GeminiTranslator) TranslateStream(ctx context.Context, events <-chan schema.InternalStreamEvent, fn func(eventData []byte, isDone bool)) {
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
				fn(append([]byte("data: "), errData...), false)
				fn([]byte("\n\n"), false)
				continue

			case "delta":
				if event.Data != nil && len(event.Data.Choices) > 0 {
					choice := event.Data.Choices[0]
					var parts []Part

					if choice.Message.Content != nil {
						var text string
						json.Unmarshal(choice.Message.Content, &text)
						if text != "" {
							parts = append(parts, Part{Text: text})
						}
					}
					for _, tc := range choice.Message.ToolCalls {
						parts = append(parts, Part{
							FunctionCall: &FunctionCall{
								Name: tc.Function.Name,
								Args: tc.Function.RawArguments,
							},
						})
					}

					content := &Content{Role: "model", Parts: parts}
					if len(parts) == 0 {
						content.Parts = []Part{{Text: ""}}
					}

					var usage *UsageMetadata
					if event.Data.Usage != nil {
						usage = &UsageMetadata{
							PromptTokenCount:     event.Data.Usage.PromptTokens,
							CandidatesTokenCount: event.Data.Usage.CompletionTokens,
							TotalTokenCount:      event.Data.Usage.TotalTokens,
						}
					}

					chunk := StreamChunk{
						Candidates:    []Candidate{{Index: choice.Index, Content: content}},
						ModelVersion:  event.Data.Model,
						UsageMetadata: usage,
					}
					raw, _ := json.Marshal(chunk)
					fn(append([]byte("data: "), raw...), false)
					fn([]byte("\n\n"), false)
				}
				continue

			case "done":
				finishReason := "STOP"
				if event.Data != nil && len(event.Data.Choices) > 0 {
					finishReason = mapFinishReasonReverse(event.Data.Choices[0].FinishReason)
				}
				var usage *UsageMetadata
				if event.Data != nil && event.Data.Usage != nil {
					usage = &UsageMetadata{
						PromptTokenCount:     event.Data.Usage.PromptTokens,
						CandidatesTokenCount: event.Data.Usage.CompletionTokens,
						TotalTokenCount:      event.Data.Usage.TotalTokens,
					}
				}
				chunk := StreamChunk{
					Candidates:    []Candidate{{Index: 0, FinishReason: finishReason, Content: &Content{Role: "model", Parts: []Part{{Text: ""}}}}},
					UsageMetadata: usage,
				}
				raw, _ := json.Marshal(chunk)
				fn(append([]byte("data: "), raw...), false)
				fn([]byte("\n\n"), false)
				fn([]byte("data: [DONE]\n\n"), true)
				return
			}
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
//  REQUEST: InternalRequest → Gemini GenerateContentRequest
// ═══════════════════════════════════════════════════════════════════════════════

func (t *GeminiTranslator) TranslateToProvider(req *schema.InternalRequest) (*GenerateContentRequest, error) {
	// --- 1. System prompt → systemInstruction ---
	var systemInstruction *Content
	if len(req.SystemPrompt) > 0 {
		systemInstruction = buildSystemInstruction(req.SystemPrompt)
	}

	// --- 2. Messages → Contents ---
	contents, err := messagesToGemini(req.Messages)
	if err != nil {
		return nil, err
	}

	// --- 3. Tools → functionDeclarations ---
	var tools *ToolConfig
	if len(req.Tools) > 0 {
		tools = &ToolConfig{
			FunctionDeclarations: toolsToGemini(req.Tools),
		}
	}

	// --- 4. GenerationConfig ---
	generationConfig := buildGenerationConfig(req)

	return &GenerateContentRequest{
		Contents:          contents,
		SystemInstruction: systemInstruction,
		Tools:             tools,
		GenerationConfig:  generationConfig,
	}, nil
}

func buildSystemInstruction(raw json.RawMessage) *Content {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return &Content{
			Role:  "user", // Gemini systemInstruction 的 role 固定为 user
			Parts: []Part{{Text: text}},
		}
	}
	// 兜底：透传原始 JSON 作为 text
	return &Content{
		Role:  "user",
		Parts: []Part{{Text: string(raw)}},
	}
}

func messagesToGemini(msgs []schema.InternalMessage) ([]Content, error) {
	var contents []Content

	for _, msg := range msgs {
		content := Content{
			Parts: make([]Part, 0, 2), // 预留 text + function_call
		}

		switch msg.Role {
		case schema.RoleUser:
			content.Role = "user"
			if len(msg.ContentBlocks) > 0 {
				for _, cb := range msg.ContentBlocks {
					switch cb.Type {
					case "text":
						content.Parts = append(content.Parts, Part{Text: cb.Text})
					case "image":
						if cb.Data != "" {
							content.Parts = append(content.Parts, Part{
								InlineData: &InlineData{
									MimeType: cb.MediaType,
									Data:     cb.Data,
								},
							})
						}
					}
				}
			} else {
				var text string
				if msg.Content != nil {
					if err := json.Unmarshal(msg.Content, &text); err == nil {
						content.Parts = append(content.Parts, Part{Text: text})
					}
				}
			}

		case schema.RoleAssistant:
			content.Role = "model" // ⚠️ Gemini 用 model 不是 assistant
			if len(msg.ContentBlocks) > 0 {
				for _, cb := range msg.ContentBlocks {
					if cb.Type == "text" && cb.Text != "" {
						content.Parts = append(content.Parts, Part{Text: cb.Text})
					}
				}
			} else {
				var text string
				if msg.Content != nil {
					json.Unmarshal(msg.Content, &text)
					if text != "" {
						content.Parts = append(content.Parts, Part{Text: text})
					}
				}
			}

			// ⚠️ tool_calls → function_call parts
			for _, tc := range msg.ToolCalls {
				part := Part{
					FunctionCall: &FunctionCall{
						Name: tc.Function.Name,
						Args: tc.Function.RawArguments, // 已是 JSON 对象
					},
				}
				content.Parts = append(content.Parts, part)
			}

		case schema.RoleTool:
			// ⚠️ Gemini 将 tool 结果归为 user 消息
			content.Role = "user"
			// msg.Name 是 tool 结果文本，msg.ToolCallID 是 function 名
			funcResp := &FunctionResponse{
				Name: msg.ToolCallID, // 这里存 function 名
				Response: map[string]interface{}{
					"result": msg.Name, // 结果文本
				},
			}
			content.Parts = append(content.Parts, Part{
				FunctionResponse: funcResp,
			})
		}

		if len(content.Parts) == 0 {
			// 空消息，添加空文本避免非法
			content.Parts = append(content.Parts, Part{Text: ""})
		}

		contents = append(contents, content)
	}

	return contents, nil
}

func toolsToGemini(tools []schema.InternalTool) []FunctionDeclaration {
	var declarations []FunctionDeclaration
	for _, tool := range tools {
		if tool.Function == nil {
			continue
		}
		declarations = append(declarations, FunctionDeclaration{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  tool.Function.Parameters,
		})
	}
	return declarations
}

func buildGenerationConfig(req *schema.InternalRequest) *GenerationConfig {
	// ⚠️ 优先使用 MaxOutputTokens（Responses 协议），否则回退到 MaxTokens（CC/Anthropic 协议）
	maxTokens := req.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = req.MaxTokens
	}
	cfg := &GenerationConfig{
		MaxOutputTokens: maxTokens,
		StopSequences:   req.StopSequences,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		TopK:            req.TopK,
	}

	if req.ResponseFormat != nil {
		switch req.ResponseFormat.Type {
		case "json_object", "json_schema":
			cfg.ResponseMimeType = "application/json"
		case "text":
			cfg.ResponseMimeType = "text/plain"
		}
	}

	return cfg
}

// ═══════════════════════════════════════════════════════════════════════════════
//  RESPONSE: Gemini Response → InternalResponse
// ═══════════════════════════════════════════════════════════════════════════════

func (t *GeminiTranslator) TranslateFromProvider(raw json.RawMessage) (*schema.InternalResponse, error) {
	var resp GenerateContentResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}

	if len(resp.Candidates) == 0 {
		return nil, fmt.Errorf("gemini response has no candidates")
	}

	candidate := resp.Candidates[0]

	// --- 1. 从 parts 中提取文本和 function_calls（兼容性 5） ---
	var choiceMessage schema.InternalMessage
	choiceMessage.Role = schema.RoleAssistant

	var textParts []string
	var toolCalls []schema.InternalToolCall
	var contentBlocks []schema.InternalContentBlock

	if candidate.Content != nil {
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				textParts = append(textParts, part.Text)
				contentBlocks = append(contentBlocks, schema.InternalContentBlock{
					Type: "text",
					Text: part.Text,
				})
			}
			if part.InlineData != nil {
				contentBlocks = append(contentBlocks, schema.InternalContentBlock{
					Type:      "image",
					Data:      part.InlineData.Data,
					MediaType: part.InlineData.MimeType,
				})
			}
			if part.FunctionCall != nil {
				// ⚠️ args 是对象，需 Marshal 为字符串
				argsJSON, _ := json.Marshal(part.FunctionCall.Args)
				toolCalls = append(toolCalls, schema.InternalToolCall{
					ID:   fmt.Sprintf("gc_%d", len(toolCalls)), // Gemini 无 ID，网关生成
					Type: "function",
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{
						Name:         part.FunctionCall.Name,
						Arguments:    string(argsJSON),
						RawArguments: part.FunctionCall.Args,
					},
				})
			}
		}
	}

	if len(toolCalls) > 0 {
		choiceMessage.ToolCalls = toolCalls
	}

	choiceMessage.Content, _ = json.Marshal(joinText(textParts))
	if len(contentBlocks) > 0 {
		choiceMessage.ContentBlocks = contentBlocks
	}

	// --- 2. Usage 字段映射（兼容性 7） ---
	var usage *schema.InternalUsage
	if resp.UsageMetadata != nil {
		usage = &schema.InternalUsage{
			PromptTokens:     resp.UsageMetadata.PromptTokenCount,
			CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      resp.UsageMetadata.TotalTokenCount,
		}
	}

	// --- 3. FinishReason 映射（兼容性 8） ---
	finishReason := mapFinishReason(candidate.FinishReason)

	model := candidate.Content.Role
	if model == "" {
		model = resp.ModelVersion
	}
	return &schema.InternalResponse{
		ID:    "",
		Model: model,
		Choices: []schema.InternalChoice{
			{
				Index:        candidate.Index,
				Message:      choiceMessage,
				FinishReason: finishReason,
			},
		},
		Usage:  usage,
		Object: "chat.completion",
	}, nil
}

func mapFinishReason(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY":
		return "content_filter"
	case "RECITATION":
		return "content_filter"
	case "BLOCKLIST":
		return "content_filter"
	case "PROHABITED_CONTENT":
		return "content_filter"
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

// @AI_GUARD: GEMINI_TRANSLATE_STREAM_EVENT - 上游 Gemini SSE 事件 → InternalStreamEvent（流式入口）
// @CONSTRAINT: 签名必须为 TranslateStreamEvent(json.RawMessage)，与 handleStreamRequest 类型断言一致
//   - Candidates[0].Content.Parts 中提取 text 和 functionCall
//   - usageMetadata 提取 token 统计
//   - finishReason 映射：STOP→stop, MAX_TOKENS→length, 其他→stop
//
// @RELATED: anthropic/translator.go TranslateStreamEvent, quick.go/gateway.go handleStreamRequest
func (t *GeminiTranslator) TranslateStreamEvent(raw json.RawMessage) *schema.InternalStreamEvent {
	var chunk StreamChunk
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return &schema.InternalStreamEvent{
			Type: "error",
			Error: &schema.StreamError{
				Message: "parse error: " + err.Error(),
				Code:    400,
			},
		}
	}

	if len(chunk.Candidates) == 0 {
		return nil
	}

	candidate := chunk.Candidates[0]

	// --- 提取文本和 function_calls（即使有 finish_reason 也要提取，末 chunk 可能含文本） ---
	var deltaContent json.RawMessage
	var toolCalls []schema.InternalToolCall

	if candidate.Content != nil {
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				deltaContent, _ = json.Marshal(part.Text)
			}
			if part.FunctionCall != nil {
				argsJSON, _ := json.Marshal(part.FunctionCall.Args)
				toolCalls = append(toolCalls, schema.InternalToolCall{
					ID:   fmt.Sprintf("gc_%d", len(toolCalls)),
					Type: "function",
					Function: struct {
						Name         string          `json:"name"`
						Arguments    string          `json:"arguments"`
						RawArguments json.RawMessage `json:"-"`
					}{
						Name:         part.FunctionCall.Name,
						Arguments:    string(argsJSON),
						RawArguments: part.FunctionCall.Args,
					},
				})
			}
		}
	}

	var msg schema.InternalMessage
	msg.Role = schema.RoleAssistant
	if deltaContent != nil {
		msg.Content = deltaContent
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}

	// --- Usage 如果有 ---
	var usage *schema.InternalUsage
	if chunk.UsageMetadata != nil {
		usage = &schema.InternalUsage{
			PromptTokens:     chunk.UsageMetadata.PromptTokenCount,
			CompletionTokens: chunk.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      chunk.UsageMetadata.TotalTokenCount,
		}
	}

	// --- 结束条件 ---
	if candidate.FinishReason != "" {
		return &schema.InternalStreamEvent{
			Type: "done",
			Data: &schema.InternalStreamChunk{
				ID:    "",
				Model: chunk.ModelVersion,
				Choices: []schema.InternalChoice{
					{
						Index:        candidate.Index,
						Message:      msg,
						FinishReason: mapFinishReason(candidate.FinishReason),
					},
				},
				Usage: usage,
			},
		}
	}

	return &schema.InternalStreamEvent{
		Type: "delta",
		Data: &schema.InternalStreamChunk{
			ID:    "",
			Model: chunk.ModelVersion,
			Choices: []schema.InternalChoice{
				{
					Index:        candidate.Index,
					Message:      msg,
					FinishReason: "",
				},
			},
			Usage: usage,
		},
	}
}

func (t *GeminiTranslator) TranslateStreamToCCSSE(ctx context.Context, events <-chan json.RawMessage, fn func(data []byte, isDone bool)) {
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

func (t *GeminiTranslator) TranslateError(err *schema.StreamError) json.RawMessage {
	gemErr := GeminiError{
		Error: &ErrorDetail{
			Code:    err.Code,
			Message: err.Message,
			Status:  err.Type,
		},
	}
	data, _ := json.Marshal(gemErr)
	return data
}

// ToCCStreamChunk 复用 anthropic 包中的通用函数
// 注意：这里在 gemini 包内定义，因为不能 cross-package 引用
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

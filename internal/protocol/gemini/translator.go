package gemini

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

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
            Role: "user", // Gemini systemInstruction 的 role 固定为 user
            Parts: []Part{{Text: text}},
        }
	}
	// 兜底：透传原始 JSON 作为 text
	return &Content{
        Role: "user",
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
            // 提取文本
            var text string
            if msg.Content != nil {
                if err := json.Unmarshal(msg.Content, &text); err == nil {
                    content.Parts = append(content.Parts, Part{Text: text})
                }
            }

        case schema.RoleAssistant:
            content.Role = "model" // ⚠️ Gemini 用 model 不是 assistant
            var text string
            if msg.Content != nil {
                json.Unmarshal(msg.Content, &text)
                if text != "" {
                    content.Parts = append(content.Parts, Part{Text: text})
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
	cfg := &GenerationConfig{
        MaxOutputTokens: req.MaxOutputTokens,
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

	if candidate.Content != nil {
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				textParts = append(textParts, part.Text)
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

	choiceMessage.Content = json.RawMessage(`"` + joinText(textParts) + `"`)

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

	return &schema.InternalResponse{
		ID:    "",
		Model: candidate.Content.Role,
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

	// --- 检查结束条件 ---
	if candidate.FinishReason != "" {
		return &schema.InternalStreamEvent{
            Type: "done",
            Data: &schema.InternalStreamChunk{
                Choices: []schema.InternalChoice{
                    {
                        Index:        candidate.Index,
                        FinishReason: mapFinishReason(candidate.FinishReason),
                    },
                },
            },
        }
	}

	// --- 提取文本和 function_calls ---
	var deltaContent json.RawMessage
	var toolCalls []schema.InternalToolCall

	if candidate.Content != nil {
		for _, part := range candidate.Content.Parts {
            if part.Text != "" {
                deltaContent = json.RawMessage(`"` + part.Text + `"`)
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

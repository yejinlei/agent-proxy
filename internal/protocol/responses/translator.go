package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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

// @AI_GUARD: RESPONSES_TRANSLATE_REQUEST - OpenAI Responses → InternalRequest（Central Schema 入口）
// @CONSTRAINT: 消息格式转换必须经过 Central Schema，禁止直接翻译到其他协议
//   - instructions → SystemPrompt
//   - input 数组逐条转换（支持文本/图片/文件等多种类型）
//   - 新增字段映射时必须同步修改 ResponseRequest 结构体
//
// @RELATED: chatcompletion/translator.go TranslateRequest, anthropic/translator.go TranslateRequest
func (t *ResponsesTranslator) TranslateRequest(ctx context.Context, rawReq json.RawMessage) (*schema.InternalRequest, error) {
	var req ResponseRequest
	if err := json.Unmarshal(rawReq, &req); err != nil {
		return nil, err
	}

	// --- 1. Instructions → SystemPrompt ---
	var systemContent json.RawMessage
	if req.Instructions != "" {
		systemContent, _ = json.Marshal(req.Instructions)
	}

	// --- 2. Input → Messages ---
	messages := inputToMessages(InputToItems(req.Input))

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
		// type 为空时默认 "message"（部分客户端省略 type 字段）
		itemType := item.Type
		if itemType == "" {
			itemType = "message"
		}
		if itemType != "message" {
			continue
		}

		msg := schema.InternalMessage{Role: schema.Role(item.Role)}

		// 提取 text + 内容块
		var text string
		var textParts []string
		var contentBlocks []schema.InternalContentBlock
		switch c := item.Content.(type) {
		case string:
			text = c
		case []interface{}:
			// content blocks
			for _, cb := range c {
				block, ok := cb.(map[string]interface{})
				if !ok {
					continue
				}
				switch block["type"] {
				case "output_text", "input_text":
					if t, ok := block["text"].(string); ok {
						textParts = append(textParts, t)
						contentBlocks = append(contentBlocks, schema.InternalContentBlock{
							Type: "text",
							Text: t,
						})
					}
				case "input_image":
					var icb schema.InternalContentBlock
					icb.Type = "image"
					if source, ok := block["source"].(map[string]interface{}); ok {
						switch source["type"] {
						case "base64":
							if data, ok := source["data"].(string); ok {
								icb.Data = data
							}
							if mediaType, ok := source["media_type"].(string); ok {
								icb.MediaType = mediaType
							}
						case "url":
							if url, ok := source["url"].(string); ok {
								icb.URL = url
							}
						}
					}
					contentBlocks = append(contentBlocks, icb)
				}
			}
			text = joinText(textParts)
		}
		msg.Content, _ = json.Marshal(text)
		if len(contentBlocks) > 0 {
			msg.ContentBlocks = contentBlocks
		}

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

// @AI_GUARD: RESPONSES_TRANSLATE_RESPONSE - InternalResponse → OpenAI Response（Central Schema 出口）
// @CONSTRAINT: 必须正确映射 InternalResponse 到 OpenAI Responses 原生响应格式
//   - ContentBlocks 转换为 output 数组（ContentBlock type 必须为 "output_text"/"output_image"，不能用 "text"/"image"）
//   - ⚠️ status 必须从 finish_reason 动态映射，不能硬编码为 "completed" —
//     例如 cancelled/tool_calls 等非 stop 原因需要映射为对应 status，否则 Codex 显示异常
//   - output[] 必须为数组，含 type:"message" 的 OutputItem
//   - ⚠️ usage 必须为非空对象（InputTokens/OutputTokens/TotalTokens），不能为 nil — Codex 客户端会校验
//
// @RELATED: chatcompletion/translator.go TranslateResponse, anthropic/translator.go TranslateResponse,
//
//	quick.go fixNullUsageInResponse (流式版本 usage 兜底)
//
// @REASON: 历史血泪教训 - v0.2.68 修复：response status 硬编码导致非 stop 场景状态错误，
//
//	影响 Codex 工具调用与中断场景的正确处理
func (t *ResponsesTranslator) TranslateResponse(resp *schema.InternalResponse) (json.RawMessage, error) {
	var contentBlocks []ContentBlock

	for _, choice := range resp.Choices {
		if len(choice.Message.ContentBlocks) > 0 {
			for _, cb := range choice.Message.ContentBlocks {
				switch cb.Type {
				case "text":
					if cb.Text != "" {
						contentBlocks = append(contentBlocks, ContentBlock{Type: "output_text", Text: cb.Text})
					}
				case "image":
					var source map[string]interface{}
					if cb.Data != "" {
						source = map[string]interface{}{
							"type":       "base64",
							"data":       cb.Data,
							"media_type": cb.MediaType,
						}
					} else if cb.URL != "" {
						source = map[string]interface{}{
							"type": "url",
							"url":  cb.URL,
						}
					}
					if source != nil {
						contentBlocks = append(contentBlocks, ContentBlock{Type: "output_image", Source: source})
					}
				}
			}
		} else {
			var text string
			if choice.Message.Content != nil {
				json.Unmarshal(choice.Message.Content, &text)
			}
			if text != "" {
				contentBlocks = append(contentBlocks, ContentBlock{Type: "output_text", Text: text})
			}
		}

		for _, tc := range choice.Message.ToolCalls {
			contentBlocks = append(contentBlocks, ContentBlock{
				Type: "tool_call",
				ID:   tc.ID,
				Name: tc.Function.Name,
				Input: func(tc schema.InternalToolCall) map[string]interface{} {
					var m map[string]interface{}
					json.Unmarshal(tc.Function.RawArguments, &m)
					return m
				}(tc),
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

// mapResponsesStatus 将 Responses API 的 response.status 映射为 finish_reason
//
//	"completed"  → "stop"
//	"incomplete" → "length"
//	"cancelled"  → "stop"
func mapResponsesStatus(status string, stopReason string) string {
	if stopReason != "" {
		return mapStopReason(stopReason)
	}
	switch status {
	case "incomplete":
		return "length"
	default:
		return "stop"
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
//  STREAM: InternalStreamEvents → Responses SSE
// ═══════════════════════════════════════════════════════════════════════════════

// @AI_GUARD: RESPONSES_TRANSLATE_STREAM - InternalStreamEvent → OpenAI Responses SSE（流式出口）
// @CONSTRAINT: Codex 严格要求 Responses API 事件序列 + stream closed 完成信号：
//   - 纯文本最简序列：
//     response.created → response.output_item.added(msg, output_index=0) →
//     response.output_text.delta* → response.output_text.done →
//     response.output_item.done(msg) → response.completed → event: done + data: [DONE]
//   - 函数调用事件（output_index=1..N）：
//     response.output_item.added(func, output_index=1) →
//     response.function_call_arguments.delta* →
//     response.function_call_arguments.done
//   - ⚠️ 禁止发送 response.in_progress / response.content_part.added/done /
//     sequence_number 等中间事件或字段，Codex 解析器报序列异常
//   - ⚠️ Responses 出口必须尾部发送 event: done\ndata: [DONE]\n\n 作为"流关闭"信号
//     （response.completed 仅标识 response 对象完成，不等价于 SSE 流结束）
//   - response.created/completed 事件数据必须用 "response" 字段（非 "data" 字段）
//   - response.completed 的 response.output[] 必须包含累积完整内容（message + function_call items）
//   - ⚠️ output_item.added 必须在 delta 之前（紧跟 response.created），确保 item 已注册
//   - ⚠️ output_item.done 必须携带最终完整 content（非空 content 数组）
//   - ⚠️ output_text.done 必须包含 text 字段（完整累积文本）
//   - channel 关闭或 ctx.Done() 时必须补发完整结束序列再发 event:done/[DONE]
//   - ⚠️ 所有 SSE 事件必须单字节切片原子写入 fn()，心跳已在 quick.go/gateway.go 中针对
//     Responses 协议通过 newDummyHeartbeat 全程禁用（ping 事件破坏 Codex 解析状态机）
//
// @RELATED: chatcompletion/translator.go TranslateStream, anthropic/translator.go TranslateStream,
//
//	quick.go handleStreamRequest（禁用心跳）, sse_heartbeat.go newDummyHeartbeat
//
// @REASON: 历史血泪教训 - v0.2.78 之前版本：
//  1. 非标准事件 response.output_delta / 缺少 response.completed → Codex 静默丢弃 → 超时
//  2. 多余中间事件 in_progress/content_part/output_item.done + sequence_number → 序列异常
//  3. 心跳 ping 事件插入 → Codex 解析状态机崩 → stream closed before response.completed
//  4. 缺 event: done + data: [DONE] 尾部 → Codex 认为 stream 未正常闭合
//  5. v0.2.79 遗漏 message 类型 output_item.added/done 事件对 → stream closed before response.completed
func (t *ResponsesTranslator) TranslateStream(ctx context.Context, events <-chan schema.InternalStreamEvent, fn func(eventData []byte, isDone bool)) {
	now := time.Now().UnixNano()
	responseID := fmt.Sprintf("resp_%d", now)
	var accumulatedText strings.Builder
	var lastModel string
	var lastUsage *Usage
	createdSent := false
	itemAdded := false
	textDoneSent := false
	messageID := fmt.Sprintf("msg_%d", now)

	type funcCallState struct {
		ID         string
		Name       string
		CallID     string
		OutputIdx  int
		argsBuffer strings.Builder
		addedSent  bool
	}
	funcCalls := make(map[string]*funcCallState)
	funcCallOrder := make([]string, 0)
	nextFuncOutputIdx := 1

	getFC := func(tc schema.InternalToolCall, tcIndexInDelta int) *funcCallState {
		key := tc.ID
		if key == "" {
			key = fmt.Sprintf("synth-%d-%d", tcIndexInDelta, nextFuncOutputIdx)
		}
		fc, ok := funcCalls[key]
		if !ok {
			name := tc.Function.Name
			callID := tc.ID
			if callID == "" {
				callID = fmt.Sprintf("call_%d_%s", nextFuncOutputIdx, responseID[5:])
			}
			fc = &funcCallState{
				ID:        key,
				Name:      name,
				CallID:    callID,
				OutputIdx: nextFuncOutputIdx,
			}
			nextFuncOutputIdx++
			funcCalls[key] = fc
			funcCallOrder = append(funcCallOrder, key)
		}
		return fc
	}

	sendSSE := func(eventType string, data interface{}) {
		raw, _ := json.Marshal(data)
		buf := make([]byte, 0, len("event: \ndata: ")+len(raw)+len("\n\n"))
		buf = append(buf, []byte("event: "+eventType+"\ndata: ")...)
		buf = append(buf, raw...)
		buf = append(buf, '\n', '\n')
		fn(buf, false)
	}

	sendDoneSSE := func() {
		buf := []byte("event: done\ndata: [DONE]\n\n")
		fn(buf, true)
	}

	sendOutputItemAddedMsg := func() {
		if itemAdded {
			return
		}
		itemAdded = true
		sendSSE("response.output_item.added", map[string]interface{}{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item": map[string]interface{}{
				"id":      messageID,
				"type":    "message",
				"role":    "assistant",
				"status":  "in_progress",
				"content": []interface{}{},
			},
		})
	}

	sendCreated := func() {
		if createdSent {
			return
		}
		createdSent = true
		sendSSE("response.created", map[string]interface{}{
			"type": "response.created",
			"response": map[string]interface{}{
				"id":     responseID,
				"status": "in_progress",
			},
		})
		// 紧跟 response.created 发送 message 类型的 output_item.added，
		// 确保 item 在任何内容到达之前已注册（Codex 状态机要求）
		sendOutputItemAddedMsg()
	}

	sendTextDelta := func(text string) {
		sendOutputItemAddedMsg()
		sendSSE("response.output_text.delta", map[string]interface{}{
			"type":          "response.output_text.delta",
			"item_id":       messageID,
			"output_index":  0,
			"content_index": 0,
			"delta":         text,
		})
	}

	sendOutputItemAddedFunc := func(fc *funcCallState) {
		if fc.addedSent {
			return
		}
		fc.addedSent = true
		sendSSE("response.output_item.added", map[string]interface{}{
			"type":         "response.output_item.added",
			"output_index": fc.OutputIdx,
			"item": map[string]interface{}{
				"id":        fc.CallID,
				"type":      "function_call",
				"call_id":   fc.CallID,
				"name":      fc.Name,
				"status":    "in_progress",
				"arguments": map[string]interface{}{},
			},
		})
	}

	sendFuncArgsDelta := func(fc *funcCallState, delta string) {
		if delta == "" {
			return
		}
		sendSSE("response.function_call_arguments.delta", map[string]interface{}{
			"type":         "response.function_call_arguments.delta",
			"item_id":      fc.CallID,
			"output_index": fc.OutputIdx,
			"delta":        delta,
		})
	}

	sendCompleted := func() {
		sendCreated()

		finalText := accumulatedText.String()

		// 确保 message 类型的 output_item.added 已发送
		sendOutputItemAddedMsg()

		if !textDoneSent {
			textDoneSent = true
			sendSSE("response.output_text.done", map[string]interface{}{
				"type":          "response.output_text.done",
				"item_id":       messageID,
				"output_index":  0,
				"content_index": 0,
				"text":          finalText,
			})
		}

		// 发送 message 类型的 output_item.done
		finalMsgItem := map[string]interface{}{
			"id":     messageID,
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []interface{}{
				map[string]interface{}{
					"type": "output_text",
					"text": finalText,
				},
			},
		}
		sendSSE("response.output_item.done", map[string]interface{}{
			"type":         "response.output_item.done",
			"output_index": 0,
			"item":         finalMsgItem,
		})

		for _, key := range funcCallOrder {
			fc := funcCalls[key]
			if !fc.addedSent {
				sendOutputItemAddedFunc(fc)
			}
			finalArgs := fc.argsBuffer.String()
			if finalArgs == "" {
				finalArgs = "{}"
			}
			sendSSE("response.function_call_arguments.done", map[string]interface{}{
				"type":         "response.function_call_arguments.done",
				"item_id":      fc.CallID,
				"output_index": fc.OutputIdx,
				"name":         fc.Name,
				"arguments":    finalArgs,
			})
		}

		output := make([]interface{}, 0, 1+len(funcCallOrder))
		output = append(output, finalMsgItem)

		for _, key := range funcCallOrder {
			fc := funcCalls[key]
			var argsObj interface{} = map[string]interface{}{}
			raw := fc.argsBuffer.String()
			if raw != "" {
				_ = json.Unmarshal([]byte(raw), &argsObj)
			}
			output = append(output, map[string]interface{}{
				"id":        fc.CallID,
				"type":      "function_call",
				"call_id":   fc.CallID,
				"name":      fc.Name,
				"status":    "completed",
				"arguments": argsObj,
			})
		}

		respPayload := map[string]interface{}{
			"id":     responseID,
			"status": "completed",
			"output": output,
		}
		if lastModel != "" {
			respPayload["model"] = lastModel
		}
		if lastUsage != nil {
			respPayload["usage"] = lastUsage
		} else {
			respPayload["usage"] = &Usage{InputTokens: 0, OutputTokens: 0, TotalTokens: 0}
		}
		sendSSE("response.completed", map[string]interface{}{
			"type":     "response.completed",
			"response": respPayload,
		})

		sendDoneSSE()
	}

	for {
		select {
		case <-ctx.Done():
			sendCompleted()
			return
		case event, ok := <-events:
			if !ok {
				sendCompleted()
				return
			}

			switch event.Type {
			case "error":
				sendCreated()
				errData := t.TranslateError(event.Error)
				buf := make([]byte, 0, len("event: response.failed\ndata: ")+len(errData)+len("\n\n"))
				buf = append(buf, []byte("event: response.failed\ndata: ")...)
				buf = append(buf, errData...)
				buf = append(buf, '\n', '\n')
				fn(buf, false)
				sendDoneSSE()
				continue

			case "start":
				if event.Data != nil && event.Data.Model != "" {
					lastModel = event.Data.Model
				}
				sendCreated()
				continue

			case "delta":
				sendCreated()
				if event.Data != nil && len(event.Data.Choices) > 0 {
					choice := event.Data.Choices[0]

					if choice.Message.Content != nil {
						var text string
						json.Unmarshal(choice.Message.Content, &text)
						if text != "" {
							sendTextDelta(text)
							accumulatedText.WriteString(text)
						}
					}

					for i, tc := range choice.Message.ToolCalls {
						fc := getFC(tc, i)
						sendOutputItemAddedFunc(fc)
						if tc.Function.Arguments != "" {
							sendFuncArgsDelta(fc, tc.Function.Arguments)
							fc.argsBuffer.WriteString(tc.Function.Arguments)
						}
					}
				}
				continue

			case "done":
				if event.Data != nil {
					if event.Data.Model != "" {
						lastModel = event.Data.Model
					}
					if event.Data.Usage != nil {
						lastUsage = &Usage{
							InputTokens:  event.Data.Usage.PromptTokens,
							OutputTokens: event.Data.Usage.CompletionTokens,
							TotalTokens:  event.Data.Usage.TotalTokens,
						}
					}
				}
				sendCompleted()
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
				Type:    "message",
				Role:    "user",
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

		// 优先使用 ContentBlocks（含图片等多模态内容），否则回退到纯文本
		if len(msg.ContentBlocks) > 0 {
			contentBlocks := buildResponsesContentBlocks(msg.ContentBlocks)
			// 合并 tool_call blocks
			for _, tc := range msg.ToolCalls {
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
			// 回退：从 msg.Content 读取纯文本
			var text string
			if msg.Content != nil {
				json.Unmarshal(msg.Content, &text)
			}
			if len(msg.ToolCalls) > 0 {
				contentBlocks := []ContentBlock{{Type: "input_text", Text: text}}
				for _, tc := range msg.ToolCalls {
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
				item.Content = ContentBlock{Type: "input_text", Text: text}
			}
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

// buildResponsesContentBlocks 将 InternalContentBlock 转为 Responses 请求内容块
// 文本 → {type:"input_text", text:"..."}
// 图片 → {type:"input_image", source:{type:"base64"|"url", data/url, media_type}}
func buildResponsesContentBlocks(blocks []schema.InternalContentBlock) []ContentBlock {
	var result []ContentBlock
	for _, cb := range blocks {
		switch cb.Type {
		case "text":
			result = append(result, ContentBlock{Type: "input_text", Text: cb.Text})
		case "image":
			var source map[string]interface{}
			if cb.Data != "" {
				source = map[string]interface{}{
					"type":       "base64",
					"data":       cb.Data,
					"media_type": cb.MediaType,
				}
			} else if cb.URL != "" {
				source = map[string]interface{}{
					"type": "url",
					"url":  cb.URL,
				}
			}
			if source != nil {
				result = append(result, ContentBlock{Type: "input_image", Source: source})
			}
		}
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
	var contentBlocks []schema.InternalContentBlock

	for _, item := range resp.Output {
		if item.Type != "message" {
			continue
		}
		for _, block := range item.Content {
			switch block.Type {
			case "output_text":
				textParts = append(textParts, block.Text)
				contentBlocks = append(contentBlocks, schema.InternalContentBlock{
					Type: "text",
					Text: block.Text,
				})
			case "output_image":
				cb := schema.InternalContentBlock{Type: "image"}
				if block.Source != nil {
					switch block.Source["type"] {
					case "base64":
						cb.Data, _ = block.Source["data"].(string)
						cb.MediaType, _ = block.Source["media_type"].(string)
					case "url":
						cb.URL, _ = block.Source["url"].(string)
					}
				}
				contentBlocks = append(contentBlocks, cb)
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

	choiceMessage.Content, _ = json.Marshal(joinText(textParts))
	if len(contentBlocks) > 0 {
		choiceMessage.ContentBlocks = contentBlocks
	}

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

// @AI_GUARD: RESPONSES_TRANSLATE_STREAM_EVENT - 上游 Responses SSE 事件 → InternalStreamEvent（流式入口）
// @CONSTRAINT: 签名与 anthropic/gemini 不同（*StreamEvent vs json.RawMessage），
//
//	调用方需通过类型断言兼容处理，不可直接统一签名（会破坏现有逻辑）
//	- response.output_delta: 提取 delta.content[0].text
//	- response.content_block_delta: 提取 delta.text
//	- response.completed: 提取 usage + finish_reason
//
// @RELATED: anthropic/translator.go TranslateStreamEvent, gemini/translator.go TranslateStreamEvent
// @REASON: 历史遗留 - Responses 翻译器使用强类型 *StreamEvent，与其他翻译器的 json.RawMessage 不一致
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
								Content: func() json.RawMessage { b, _ := json.Marshal(data.Delta.Text); return b }(),
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
				Choices: []schema.InternalChoice{
					{FinishReason: mapResponsesStatus(data.Status, data.StopReason)},
				},
				Usage: usage,
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
						Content: func() json.RawMessage { b, _ := json.Marshal(deltaText); return b }(),
					},
				},
			},
		},
	}
}

// TranslateStreamToCCSSE 将 Responses 流式输出翻译为 CC 格式 SSE
func (t *ResponsesTranslator) TranslateStreamToCCSSE(ctx context.Context, events <-chan *StreamEvent, fn func(data []byte, isDone bool)) {
	// writeData 原子写入 SSE data 事件（避免心跳 goroutine 在 fn 间隙插入打断事件）
	writeData := func(data []byte, isDone bool) {
		buf := make([]byte, 0, len("data: ")+len(data)+2)
		buf = append(buf, []byte("data: ")...)
		buf = append(buf, data...)
		buf = append(buf, '\n', '\n')
		fn(buf, isDone)
	}
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
				writeData(append([]byte("{\"error\":"), errData...), false)
				continue
			}

			data := ToCCStreamChunk(internalEvent.Data)
			writeData(data, false)
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

package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
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

	// @AI_GUARD: RESPONSES_EMPTY_INPUT_USER_FALLBACK - 空 input 注入 user 消息
	// @CONSTRAINT: 若 messages 为空（Codex prewarm 请求 input:[] / generate:false），
	//   必须注入一条最小 user 消息兜底，否则上游（Sensenova）只有 system 消息时报
	//   400 "Failed to build prompt: No user query found in messages."
	// @RELATED: inputToMessages
	// @REASON: v0.2.109 — Codex 连接预热发 input:[] 请求，翻译后只有 system 无 user，
	//   Sensenova 拒绝。注入中性 user 消息让上游接受请求并正常返回（content 可为空串）。
	if len(messages) == 0 {
		emptyContent, _ := json.Marshal("")
		messages = []schema.InternalMessage{{
			Role:    schema.RoleUser,
			Content: emptyContent,
		}}
	}

	// @CODEX-DEBUG v0.2.98：Codex 入站请求形状诊断（生产可见，用 [CODEX-DEBUG] 前缀便于 grep）
	log.Printf("[CODEX-DEBUG] TranslateRequest: model=%q instructions_len=%d input_items=%d messages=%d tools=%d stream=%v raw_len=%d",
		req.Model, len(req.Instructions), len(InputToItems(req.Input)), len(messages), len(req.Tools), req.Stream, len(rawReq))

	// input items 逐条形状摘要。
	// 上游请求体日志被 formatJSON 截到 20KB+8KB（quick.go），大请求的 messages 中段
	// 完整内容进不了日志；排查"模型最后一轮决定收口而不再调工具"这类问题时，
	// 看不到历史里到底有哪些 function_call / function_call_output 往返，无法判断
	// 是模型认为已读过、还是被前面的 tool result 带偏。此处只打形状不打印文本身，
	// 避免日志膨胀。
	items := InputToItems(req.Input)
	log.Printf("[CODEX-DEBUG] input items digest: %s", responsesItemDigest(items))

	// --- 3. Tools → InternalTools ---
	// @AI_GUARD: RESPONSES_FILTER_BUILTIN_TOOLS - 跳过客户端内置工具
	// @CONSTRAINT: 只把 type=="function" 且 name 非空的 tool 转发给上游；其余（tool_search/web_search
	//   等客户端执行的内置工具）必须丢弃，否则会产出 function.name 为空的非法定义，
	//   上游报 400 "Invalid request format"
	// @RELATED: types.go Tool.UnmarshalJSON（空 name 回退分支）
	// @REASON: v0.2.109 — Codex 经 WS 发来的 tools 含 tool_search/web_search 内置工具，
	//   UnmarshalJSON 因 t.Name=="" 走 else 分支只设 Type/Parameters，name 留空；
	//   翻译器若原样包成 function 转发，Sensenova 报 Invalid request format。
	var tools []schema.InternalTool
	for _, tool := range req.Tools {
		if tool.Type != "function" || tool.Name == "" {
			continue
		}
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

	// droppedTypes 统计无法映射到 InternalMessage 的 item 类型：
	// function_call / function_call_output 现在有专门分支，只有 reasoning 等
	// 仍会被忽略——必须留日志，否则 "26→18 条"这类静默丢失永远无法归因。
	droppedTypes := map[string]int{}

	for _, item := range items {
		// type 为空时默认 "message"（部分客户端省略 type 字段）
		itemType := item.Type
		if itemType == "" {
			itemType = "message"
		}

		switch itemType {
		case "message":
			// @AI_GUARD: RESPONSES_INPUT_ITEM_TYPES - 非 message item 不再整条丢弃
			// @CONSTRAINT: 每类 Responses input item 必须显式落在某个 case 上；新增类型时
			//   必须加分支或确认 default 的丢弃是预期的（会打 [CODEX-DEBUG] 日志）。
			// @REASON: v0.2.119 — 原实现 itemType != "message" 直接 continue，Codex 会话里
			//   assistant 的 function_call 与 function_call_output 全部消失（26→18 / 28→20 / 30→22），
			//   上游模型看不到自己发起过的工具调用和真实输出，只能顺着幻觉重复。
			msg, ok := itemToMessage(item)
			if ok {
				msgs = append(msgs, msg)
			}
		case "function_call":
			msg, ok := itemToMessage(item)
			if ok {
				msgs = append(msgs, msg)
			} else {
				droppedTypes[itemType]++
			}
		case "function_call_output":
			msgs = append(msgs, functionCallOutputItemToMessage(item))
		default:
			droppedTypes[itemType]++
		}
	}

	if len(droppedTypes) > 0 {
		names := make([]string, 0, len(droppedTypes))
		for name, n := range droppedTypes {
			names = append(names, fmt.Sprintf("%s=%d", name, n))
		}
		sort.Strings(names)
		log.Printf("[CODEX-DEBUG] inputToMessages: input_items=%d messages=%d dropped=%s",
			len(items), len(msgs), strings.Join(names, ","))
	}

	return msgs
}

// responsesItemDigest 生成 input items 的逐条形状摘要（只打形状，不打印文本身）。
//
// Codex 工具循环类问题的关键证据在 items 的形状序列里——历史中 function_call /
// function_call_output 的 call_id 是否成对出现、最后一次工具结果之后又跟了什么。
// 上游请求体日志被 formatJSON 截断（quick.go 20KB+8KB），大请求中段内容进不了日志，
// 因此这里在翻译入口就地打一份形状摘要。只报长度与标识，不输出文本/参数原文，
// 避免日志膨胀与敏感内容外泄。
//
// 字段的取值分支必须与 itemToMessage / functionCallOutputItemToMessage 一致，
// 否则摘要显示的 call_id 与真正发出的不一致，会把排查引向错误方向。
func responsesItemDigest(items []InputItem) string {
	if len(items) == 0 {
		return "(no items)"
	}
	parts := make([]string, 0, len(items))
	for i, item := range items {
		itemType := item.Type
		if itemType == "" {
			itemType = "message"
		}
		p := fmt.Sprintf("%d:%s", i, itemType)
		switch itemType {
		case "message":
			if item.Role != "" {
				p += " role=" + item.Role
			} else {
				// 空 role 会走 itemToMessage 的默认分支，摘要标出来便于确认
				p += " role=<default>"
			}
			switch c := item.Content.(type) {
			case string:
				p += fmt.Sprintf(" text_bytes=%d", len(c))
			case []interface{}:
				var blockTypes []string
				textBytes := 0
				for _, cb := range c {
					m, ok := cb.(map[string]interface{})
					if !ok {
						continue
					}
					bt, _ := m["type"].(string)
					if bt == "" {
						bt = "?"
					}
					blockTypes = append(blockTypes, bt)
					if bt == "input_text" || bt == "output_text" || bt == "text" {
						if ts, ok := m["text"].(string); ok {
							textBytes += len(ts)
						}
					}
				}
				if len(blockTypes) > 0 {
					p += " blocks=" + strings.Join(blockTypes, ",")
				}
				p += fmt.Sprintf(" text_bytes=%d", textBytes)
			default:
				p += fmt.Sprintf(" content_bytes=%d", jsonLen(item.Content))
			}
			if len(item.ToolCalls) > 0 {
				p += fmt.Sprintf(" tool_calls=%d", len(item.ToolCalls))
			}
		case "function_call":
			name := item.Name
			args := item.Arguments
			call := item.CallID
			if len(item.ToolCalls) > 0 {
				if item.ToolCalls[0].Name != "" {
					name = item.ToolCalls[0].Name
				}
				call = item.ToolCalls[0].ID
				if args == "" {
					args = string(jsonMarshalCompact(item.ToolCalls[0].Input))
				}
			}
			if name == "" {
				name = "?"
			}
			p += fmt.Sprintf(" name=%s call=%s args=%d", name, digestCallID(call), len(args))
		case "function_call_output":
			p += fmt.Sprintf(" call=%s out=%d", digestCallID(item.CallID), len(extractOutputText(item.Output)))
		case "reasoning":
			// 只标记存在：思考过程不透出，也不属于工具循环诊断所需信息
		default:
			p += " UNKNOWN_TYPE"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " ")
}

// responsesLogTail 取文本末尾 160 字符并压成单行，供 [CODEX-DEBUG] 汇总日志使用。
// 模型"最后一轮只回了一句就收口"的措辞是判断它为何停止调工具的唯一线索，
// 但整段输出进日志会膨胀，因此只保留尾部。所有空白（含换行）折叠成单空格，
// 保证该字段始终是日志的一行，不会被多行输出打散成多条难以归属的记录。
func responsesLogTail(s string) string {
	if len(s) > 160 {
		s = s[len(s)-160:]
	}
	return strings.Join(strings.Fields(s), " ")
}

// digestCallID 空 call_id 会被 itemToMessage 现场合成，与历史里任何
// function_call_output 都对不上——这正是悬空工具结果的成因之一，必须显式标出。
func digestCallID(id string) string {
	if id == "" {
		return "SYNTH"
	}
	if len(id) > 28 {
		return id[:16] + ".." + id[len(id)-8:]
	}
	return id
}

func jsonLen(v interface{}) int {
	if v == nil {
		return 0
	}
	b, err := json.Marshal(v)
	if err != nil {
		return -1
	}
	return len(b)
}

func jsonMarshalCompact(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

// parseResponsesImageBlock 解析 Responses 协议的 input_image 内容块，返回中枢 image 块。
// 返回 nil 表示该块没有任何可用图像数据，调用方必须跳过——写进 ContentBlocks 会变成
// 一个空 image 块，出站时被「无 Data 无 URL」的分支静默丢弃，上游永远收不到图。
//
// 两种线格式都接受，因为入站 Responses 请求的来源不止一种：
//   - OpenAI Responses API 官方格式：{type:"input_image", source:{type:"base64"|"url", ...}}
//   - Codex CLI 格式：{type:"input_image", image_url:"data:image/png;base64,...", detail:"high"}
//     （见 codex-rs/protocol/src/models.rs ContentItem::InputImage，image_url 在块顶层）
//
// Codex 分支此前完全缺失：入站只读 source，Codex 的 image_url 读不到 → 空块 → 图片
// 静默丢失，且全程无任何报错。image_url 既可能是 data URL 也可能是普通 URL，两种都处理。
// 解析出的数据统一落进 Data/MediaType/URL，与 chatcompletion.ParseCCContentBlocks 的
// data URL 拆分逻辑一致（参考已验证可用的 Anthropic 上游路径）。
func parseResponsesImageBlock(block map[string]interface{}) *schema.InternalContentBlock {
	icb := schema.InternalContentBlock{Type: "image"}
	if source, ok := block["source"].(map[string]interface{}); ok {
		switch source["type"] {
		case "base64":
			icb.Data, _ = source["data"].(string)
			icb.MediaType, _ = source["media_type"].(string)
		case "url":
			icb.URL, _ = source["url"].(string)
			icb.MediaType, _ = source["media_type"].(string)
		}
	}
	if url, ok := block["image_url"].(string); ok && url != "" {
		if strings.HasPrefix(url, "data:") {
			if comma := strings.Index(url, ","); comma > 0 {
				// "data:image/png;base64" → MediaType 取分号前的媒体类型
				mediaType := strings.TrimPrefix(url[:comma], "data:")
				if semi := strings.Index(mediaType, ";"); semi > 0 {
					mediaType = mediaType[:semi]
				}
				icb.MediaType = mediaType
				icb.Data = url[comma+1:]
			} else {
				icb.URL = url
			}
		} else {
			icb.URL = url
		}
	}
	if icb.Data == "" && icb.URL == "" {
		return nil
	}
	return &icb
}

// responsesImageDataURL 把中枢 image 块转回 Responses 顶层 image_url 字段值。
// Data 优先（base64 内联），URL 兜底。Data 为空且无 URL 时返回空串，调用方据此跳过该块。
// 注意：Responses 顶层 image_url 只有这个字段，没有 detail —— detail 是客户端到
// 服务端的方向上（Codex 请求）才带，出站响应不需要也不应重复。
func responsesImageDataURL(cb schema.InternalContentBlock) string {
	if cb.Data != "" {
		mt := cb.MediaType
		if mt == "" {
			mt = "image/png"
		}
		if strings.HasPrefix(mt, "data:") {
			// 防御：MediaType 已经是完整 data URL 前缀时直接拼，不再二次包装
			return mt + cb.Data
		}
		return "data:" + mt + ";base64," + cb.Data
	}
	return cb.URL
}

// itemToMessage 把单个 type:"message"（或 type:"function_call"）item 转为 InternalMessage。
// 返回 false 表示该 item 无可转换内容（空文本 + 无内容块 + 无工具调用）。
// @AI_GUARD: RESPONSES_INPUT_ITEM_TYPES - 工具调用有两种来源形态
// @CONSTRAINT: 优先 item.ToolCalls（CC 嵌套形态）；没有时用 item.Name + item.Arguments。
//
//	Arguments 必须保持 JSON 字符串原样，禁止 Unmarshal 成 map 再 Marshal（会丢顺序/转义，
//	且与 @AI_GUARD: RESPONSES_FC_ARGUMENTS_STRING 冲突）。
func itemToMessage(item InputItem) (schema.InternalMessage, bool) {
	msg := schema.InternalMessage{Role: schema.Role(item.Role)}
	if msg.Role == "" {
		// @AI_GUARD: RESPONSES_FC_ROLE_DEFAULT - 缺 role 的 item 必须补默认 role，否则上游 400
		// @CONSTRAINT: OpenAI 系上游强校验 messages[].role 非空；空 role → 400
		//   "field Messages[N].Role invalid, should be set"，整条流 0 delta 直接失败。
		//   function_call item（Codex 历史里的工具调用）不带 role，且工具调用只能挂在 assistant 上；
		//   message item 省略 role 时按内容块类型推断：input_text 属用户，其余视为 assistant。
		// @REASON: v0.2.119 修好「非 message item 整条丢弃」后，历史里的 function_call 第一次真正
		//   送到上游，立刻暴露这个空 role 回归（实测两次 400，全部 0 delta）。
		// @RELATED: inputToMessages 的 "function_call" 分支、functionCallOutputItemToMessage
		msg.Role = schema.Role("assistant")
		if item.Type != "function_call" {
			if blocks, ok := item.Content.([]interface{}); ok {
				for _, cb := range blocks {
					if m, ok := cb.(map[string]interface{}); ok && m["type"] == "input_text" {
						msg.Role = schema.Role("user")
						break
					}
				}
			}
		}
	}

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
			case "output_text", "input_text", "text":
				// @AI_GUARD: RESPONSES_TEXT_BLOCK_TYPES - "text" 块必须与 input_text/output_text 等价
				// @CONSTRAINT: 非 Codex 客户端常发 {"type":"text","text":...}；此前被静默丢弃 →
				//   请求变成"只有 system 没有 user" → 上游 400 "No user query found in messages."
				// @REASON: v0.2.119 健壮性修复；Codex 自身用 input_text/output_text，不受影响。
				if t, ok := block["text"].(string); ok {
					textParts = append(textParts, t)
					contentBlocks = append(contentBlocks, schema.InternalContentBlock{
						Type: "text",
						Text: t,
					})
				}
			case "input_image":
				if icb := parseResponsesImageBlock(block); icb != nil {
					contentBlocks = append(contentBlocks, *icb)
				}
			case "tool_result":
				var toolCallID string
				if tcid, ok := block["tool_call_id"].(string); ok {
					toolCallID = tcid
				}
				if toolCallID == "" {
					toolCallID = item.ToolCallID
				}
				if toolCallID == "" {
					toolCallID = item.CallID
				}
				var toolText string
				switch toolContent := block["content"].(type) {
				case string:
					toolText = toolContent
				case []interface{}:
					for _, sub := range toolContent {
						if subBlock, ok := sub.(map[string]interface{}); ok {
							if subText, ok := subBlock["text"].(string); ok {
								toolText += subText
							}
						}
					}
				}
				toolContentJSON, _ := json.Marshal(toolText)
				return schema.InternalMessage{
					Role:       schema.Role("tool"),
					ToolCallID: toolCallID,
					Content:    toolContentJSON,
				}, true
			}
		}
		text = joinText(textParts)
	}
	msg.Content, _ = json.Marshal(text)
	if len(contentBlocks) > 0 {
		msg.ContentBlocks = contentBlocks
	}

	// Tool calls：优先 item.ToolCalls（旧/CC 嵌套形态）
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

	// 标准 Responses 形态：function_call item 的 name + arguments（JSON 字符串）
	if len(item.ToolCalls) == 0 && item.Name != "" {
		args := strings.TrimSpace(item.Arguments)
		if args == "" {
			args = "{}"
		}
		callID := item.CallID
		if callID == "" {
			callID = fmt.Sprintf("call_%s_%d", item.Name, time.Now().UnixNano())
		}
		msg.ToolCalls = append(msg.ToolCalls, schema.InternalToolCall{
			ID:   callID,
			Type: "function",
			Function: struct {
				Name         string          `json:"name"`
				Arguments    string          `json:"arguments"`
				RawArguments json.RawMessage `json:"-"`
			}{
				Name:         item.Name,
				Arguments:    args,
				RawArguments: json.RawMessage(args),
			},
		})
	}

	if text == "" && len(contentBlocks) == 0 && len(msg.ToolCalls) == 0 {
		return msg, false
	}
	return msg, true
}

// functionCallOutputItemToMessage 把 type:"function_call_output" item 转为 role=tool 消息。
// @AI_GUARD: RESPONSES_INPUT_ITEM_TYPES - output 字段形态不固定，必须逐级兜底
// @CONSTRAINT: OpenAI 规范里 output 是数组（[{type:"text",text:...}] / [{type:"refusal"}]），
//
//	实际客户端也可能发字符串或对象。任何提取失败仍要产出带 call_id 的 tool 消息，
//	不能让上游出现"有调用无结果"的悬空配对。
func functionCallOutputItemToMessage(item InputItem) schema.InternalMessage {
	text := extractOutputText(item.Output)
	if text == "" {
		text = extractOutputText(item.Content)
	}
	contentJSON, _ := json.Marshal(text)
	return schema.InternalMessage{
		Role:       schema.Role("tool"),
		ToolCallID: item.CallID,
		Content:    contentJSON,
	}
}

// extractOutputText 从 function_call_output 的 output/content 字段提取纯文本。
func extractOutputText(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []interface{}:
		var parts []string
		for _, sub := range t {
			if s, ok := sub.(map[string]interface{}); ok {
				switch s["type"] {
				case "text", "output_text", "input_text":
					if st, ok := s["text"].(string); ok {
						parts = append(parts, st)
					}
				case "refusal":
					if sr, ok := s["refusal"].(string); ok {
						parts = append(parts, sr)
					}
				default:
					if nested := extractOutputText(s["content"]); nested != "" {
						parts = append(parts, nested)
					}
					if st, ok := s["text"].(string); ok {
						parts = append(parts, st)
					}
				}
			}
		}
		return joinText(parts)
	case map[string]interface{}:
		if inner := extractOutputText(t["content"]); inner != "" {
			return inner
		}
		if st, ok := t["text"].(string); ok {
			return st
		}
		b, _ := json.Marshal(t)
		return string(b)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
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
	// @AI_GUARD: RESPONSES_FINISH_REASON_LOG - finish_reason 必须在流末尾落日志
	// @CONSTRAINT: quick.go/gateway.go 的翻译流 goroutine 只保留 accumulatedUsage，
	//   上游 choice.FinishReason 在这里是唯一能被观测到的点。丢失后无法从日志区分
	//   stop / length / tool_calls / content_filter，也就无法定位"模型被截断"这类故障。
	// @REASON: v0.2.119 — 请求行长期显示 "Token: —"，Codex 幻觉循环无法归因。
	var lastFinishReason string
	var nDeltaEvents int
	var nTextDeltas int
	var nFuncArgsDeltas int
	startedAt := time.Now()
	createdSent := false
	itemAdded := false
	textDoneSent := false
	msgItemClosed := false
	messageID := fmt.Sprintf("msg_%d", now)

	type funcCallState struct {
		ID         string
		Name       string
		CallID     string
		OutputIdx  int
		argsBuffer strings.Builder
		addedSent  bool
		argsDone   bool // fc_arguments.done 已发送（防止 sendCompleted 重复发）
		itemDone   bool // function_call output_item.done 已发送（保证 added/done 配对）
	}
	funcCalls := make(map[string]*funcCallState)
	funcCallOrder := make([]string, 0)
	nextFuncOutputIdx := 1

	getFC := func(tc schema.InternalToolCall, tcIndexInDelta int) *funcCallState {
		// @AI_GUARD: RESPONSES_FC_KEYING - 无 ID 的续传 delta 必须合并回当前 fc，不能各造幽灵条目
		// @CONSTRAINT: 上游 CC 流里 tool_call 的 ID/Name 只在首个分片出现，后续分片 ID 为空。
		//   空 ID 且已有活跃 fc 时，按 index 匹配现有条目（找不到则取最后一个），继续累积参数。
		// @REASON: v0.2.109 之前空 ID 一律合成 "synth-N" 新条目 → 同一调用裂成多条，
		//   产生未登记的 output_item.added + "{}" 幽灵 fc_arguments.done，Codex 拿到空参数
		//   （pwd 工具返回空 Path）。
		key := tc.ID
		if key == "" {
			// 续传分片：合并回同一 choice 内正在流式输出的 fc（取最后登记的条目）。
			// 多并行 tool_call 场景下上游通常仍带 ID，只有纯参数续传才为空。
			if len(funcCallOrder) > 0 {
				return funcCalls[funcCallOrder[len(funcCallOrder)-1]]
			}
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
		} else if newName := tc.Function.Name; newName != "" && fc.Name == "" {
			fc.Name = newName
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
		log.Printf("[CODEX-DEBUG] TranslateStream emit: %s bytes=%d", eventType, len(buf))
	}

	sendDoneSSE := func() {
		buf := []byte("event: done\ndata: [DONE]\n\n")
		fn(buf, true)
		log.Printf("[CODEX-DEBUG] TranslateStream emit: done/[DONE] bytes=%d", len(buf))
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
		nTextDeltas++
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
				"arguments": "", // @CONSTRAINT: 必须是字符串（流式期间尚未累积参数），不能是对象——Codex 严格校验 item 类型
			},
		})
	}

	// closeFuncCall 关闭一个 function_call item：fc_arguments.done + output_item.done 各一次。
	// @AI_GUARD: RESPONSES_FC_TEARDOWN - added/done 必须严格配对
	// @AI_GUARD: RESPONSES_FC_ARGUMENTS_STRING - function_call.arguments 必须是 JSON 字符串，不能是对象
	// @CONSTRAINT: 输出侧（output_item.done.function_call.arguments / response.completed.output[].arguments）
	//   必须是原始参数字符串（如 "{\"path\":\"F:/src/x\"}"），Responses API 规范如此，Codex 工具调用
	//   harness 据此反序列化。此前用 json.Unmarshal 还原成 map 再 Marshal 成对象输出 → Codex 无法解析，
	//   整个 turn 静默丢弃（工具不执行，且不再发下一轮请求）。
	// @CONSTRAINT: 每个 opened fc 恰好一条 fc_arguments.done（argsDone 守卫）+ 一条
	//   output_item.done(function_call)（itemDone 守卫）；done 后不再接收该 fc 的 delta。
	// @REASON: v0.2.109 之前 per-fc output_item.done 从未发送（3 added / 1 done 失衡），
	//   且 sendCompleted 对空 buffer 幽灵条目补发 "{}" fc_arguments.done，与流式期间已发的
	//   真 done 重复 → Codex 先绑定空参数 → pwd 工具空 Path。
	closeFuncCall := func(fc *funcCallState) {
		if !fc.addedSent {
			sendOutputItemAddedFunc(fc)
		}
		finalArgs := fc.argsBuffer.String()
		if finalArgs == "" {
			finalArgs = "{}"
		}
		if !fc.argsDone {
			fc.argsDone = true
			sendSSE("response.function_call_arguments.done", map[string]interface{}{
				"type":         "response.function_call_arguments.done",
				"item_id":      fc.CallID,
				"output_index": fc.OutputIdx,
				"name":         fc.Name,
				"arguments":    finalArgs,
			})
		}
		if !fc.itemDone {
			fc.itemDone = true
			sendSSE("response.output_item.done", map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": fc.OutputIdx,
				"item": map[string]interface{}{
					"id":        fc.CallID,
					"type":      "function_call",
					"call_id":   fc.CallID,
					"name":      fc.Name,
					"status":    "completed",
					"arguments": finalArgs,
				},
			})
		}
	}

	// closeMessageItem 关闭 message item：output_text.done + output_item.done(message)。
	closeMessageItem := func(status string) map[string]interface{} {
		finalText := accumulatedText.String()
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
		item := map[string]interface{}{
			"id":     messageID,
			"type":   "message",
			"role":   "assistant",
			"status": status,
			"content": []interface{}{
				map[string]interface{}{
					"type": "output_text",
					"text": finalText,
				},
			},
		}
		if !msgItemClosed {
			msgItemClosed = true
			sendSSE("response.output_item.done", map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": 0,
				"item":         item,
			})
		}
		return item
	}

	sendFuncArgsDelta := func(fc *funcCallState, delta string) {
		if delta == "" {
			return
		}
		nFuncArgsDeltas++
		sendSSE("response.function_call_arguments.delta", map[string]interface{}{
			"type":         "response.function_call_arguments.delta",
			"item_id":      fc.CallID,
			"output_index": fc.OutputIdx,
			"delta":        delta,
		})
	}

	// finalizeItems 按协议顺序收尾所有已打开的 item：
	// 先关 message（output_text.done + output_item.done），再逐个关 function_call。
	// @AI_GUARD: RESPONSES_ITEM_ORDER - completed 前必须关闭所有 opened item，且 added/done 一一配对
	// @AI_GUARD: RESPONSES_FINISH_REASON_LOG - 终态响应必须带 finish_reason/incomplete_details
	// @CONSTRAINT: Codex 依赖 response.completed.response.finish_reason 判定本轮是否被截断；
	//   缺失时它无法区分"模型说完了"和"上游掐断了"。有 function_call item 时为 "function_call"，
	//   否则取上游 finish_reason（缺失回退 "stop"）。
	finalizeItems := func(status string) []interface{} {
		sendCreated() // 兜底：保证 created + msg added 存在
		msgItem := closeMessageItem(status)
		output := make([]interface{}, 0, 1+len(funcCallOrder))
		output = append(output, msgItem)
		for _, key := range funcCallOrder {
			fc := funcCalls[key]
			closeFuncCall(fc)
			var argsStr string
			if raw := fc.argsBuffer.String(); raw != "" {
				argsStr = raw
			} else {
				argsStr = "{}"
			}
			output = append(output, map[string]interface{}{
				"id":        fc.CallID,
				"type":      "function_call",
				"call_id":   fc.CallID,
				"name":      fc.Name,
				"status":    status,
				"arguments": argsStr,
			})
		}
		return output
	}

	sendCompleted := func() {
		finishReason := lastFinishReason
		if finishReason == "" {
			if len(funcCallOrder) > 0 {
				finishReason = "function_call"
			} else {
				finishReason = "stop"
			}
		}
		output := finalizeItems("completed")

		respPayload := map[string]interface{}{
			"id":            responseID,
			"status":        "completed",
			"output":        output,
			"finish_reason": finishReason,
			"incomplete_details": map[string]interface{}{
				"reason": nil,
			},
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

		// @AI_GUARD: RESPONSES_FINISH_REASON_LOG - 流结束必须落一条可 grep 的汇总日志
		// tail / fcs / took 是本次补充的字段，同样不要删：text_chars 只给总量，看不到
		// "最后一轮只回了一句就收口"的具体措辞；func_calls 为 0 时没有任何线索说明模型
		// 为什么不再调工具。tail 是模型实际输出的最后 160 字符，fcs 列出每个工具调用的
		// call_id 与参数长度，两者结合才能判断是"模型认为已读过"还是被前面的工具结果带偏。
		var fcDesc []string
		for _, k := range funcCallOrder {
			fc := funcCalls[k]
			fcDesc = append(fcDesc, fmt.Sprintf("%s/%s/%d", fc.CallID, fc.Name, fc.argsBuffer.Len()))
		}
		if len(fcDesc) == 0 {
			fcDesc = append(fcDesc, "none")
		}
		endedText := accumulatedText.String()
		log.Printf("[CODEX-DEBUG] TranslateStream END: model=%q finish_reason=%q text_chars=%d text_deltas=%d func_calls=%d n_delta_events=%d fc_args_deltas=%d took=%s usage=%v fcs=[%s] tail=%q",
			lastModel, finishReason, accumulatedText.Len(), nTextDeltas, len(funcCallOrder),
			nDeltaEvents, nFuncArgsDeltas, time.Since(startedAt).Round(time.Millisecond),
			respPayload["usage"], strings.Join(fcDesc, " "), responsesLogTail(endedText))

		sendDoneSSE()
	}

	// @AI_GUARD: RESPONSES_CREATED_EAGER - response.created 必须在上游首个事件前立即发出
	// @CONSTRAINT: 上游（sensenova）冷启动 2–4 分钟才产出首个 SSE 事件；lazy sendCreated()
	//   依赖 case "start"/"done"，静默期内完全不触发 → Codex keepalive 判定时长到达、主动关 TCP
	//   （broken pipe）。必须在 select 阻塞上游之前立即发出 response.created + output_item.added，
	//   让 Codex 在握手完成后毫秒级收到应用层信号、重置 keepalive。
	// @RELATED: ws-keepalive-fix memory（方案 B），quick.go WritePing（text 帧心跳是辅修）
	// @REASON: v0.2.113 把 WS 心跳从 ping 控制帧改为 text 帧（5s），但 sensenova 冷启动远超
	//   Codex keepalive 窗口，心跳仍不足以救活连接；日志显示 8 个 WS 连接全部 broken pipe。
	// @CONSTRAINT: sendCreated() 内部的 createdSent=true 保证后续 case "start"/"done" 不会重复发送
	//   response.created（已注册 item 也不会重复），与 lazy 路径完全兼容
	// @REASON: Anthropic 翻译器自然地在 TranslateStream 入口发送 message_start，本翻译器此前遗漏
	// @AI_GUARD: 不触碰 anthropic/translator.go，符合"涉及 claude 协议的先不要动"约定
	sendCreated()

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
				// @AI_GUARD: RESPONSES_STREAM_ERROR_TEARDOWN - 上游错误时必须补发完整结束序列
				// @CONSTRAINT: Codex 状态机要求 response.completed 作为终态标记；只发 response.error + [DONE]
				//   会让 Codex 报 "stream closed before response.completed"（见 CLAUDE.md Responses SSE 生命周期）
				// @RELATED: sendCompleted() 正常结束路径, anthropic/translator.go 错误路径
				// @REASON: v0.2.98 在限流(429 rpm exhausted)场景下 error 分支跳过 sendCompleted，
				//   导致 Codex 永远收不到 response.completed，报 stream closed before response.completed
				//   v0.2.119 在保留 completed 的前提下补发 response.error，
				//   让按事件类型分发的客户端能看到真正的错误而非"干净完成 + 空内容"。
				log.Printf("[CODEX-DEBUG] TranslateStream upstream error: status=%d type=%q message=%q",
					event.Error.Code, event.Error.Type, event.Error.Message)
				errObj := streamErrorObject(event.Error) // 单层 error 对象，供 response.error/completed 内嵌

				// 关闭所有已打开 item（message + 已出现的 fc），status=failed
				output := finalizeItems("failed")

				// 独立 response.error 事件：按事件类型分发的客户端靠它识别失败
				sendSSE("response.error", map[string]interface{}{
					"type":  "response.error",
					"error": errObj,
				})

				// 失败终态的 finish_reason 与 incomplete_details：status=failed 表示请求从未成功，
				// 不能写成 "max_output_tokens"（会让客户端把「上游 400 拒绝请求」误判成「输出太长」，
				// 走错恢复路径）。无 function_call 时 finish_reason 无真实语义，取 "stop" 占位。
				// @AI_GUARD: RESPONSES_ERROR_SHAPE - 见本分支顶部的约束注释
				errFinishReason := lastFinishReason
				if errFinishReason == "" {
					if len(funcCallOrder) > 0 {
						errFinishReason = "function_call"
					} else {
						errFinishReason = "stop"
					}
				}
				// response.completed 作为终态，携带同一 error 对象
				respPayload := map[string]interface{}{
					"id":            responseID,
					"status":        "failed",
					"output":        output,
					"error":         errObj,
					"finish_reason": errFinishReason,
					"incomplete_details": map[string]interface{}{
						"reason": nil,
					},
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
				log.Printf("[CODEX-DEBUG] TranslateStream END(err): model=%q status=failed finish_reason=%q text_chars=%d text_deltas=%d func_calls=%d n_delta_events=%d n_funcargs_deltas=%d took=%s http=%d upstream_type=%q upstream_msg=%.200s tail=%q",
					lastModel, errFinishReason, accumulatedText.Len(), nTextDeltas, len(funcCallOrder),
					nDeltaEvents, nFuncArgsDeltas, time.Since(startedAt).Round(time.Millisecond),
					event.Error.Code, event.Error.Type, event.Error.Message, responsesLogTail(accumulatedText.String()))
				sendDoneSSE()
				return

			case "start":
				if event.Data != nil && event.Data.Model != "" {
					lastModel = event.Data.Model
				}
				sendCreated()
				continue

			case "delta":
				sendCreated()
				nDeltaEvents++
				if event.Data != nil && len(event.Data.Choices) > 0 {
					choice := event.Data.Choices[0]
					if choice.FinishReason != "" {
						lastFinishReason = choice.FinishReason
					}

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
				// @AI_GUARD: RESPONSES_DONE_EMBEDDED_CONTENT - finish 分片可能携带正文/工具调用，不能丢弃
				// @CONSTRAINT: 部分上游（sensenova 等）把完整 tool_calls 塞进带 finish_reason 的
				//   最后一个 chunk；此前 done 分支只读 Model/Usage → 全部丢失。
				//   必须先按 delta 同样方式处理 Content/ToolCalls，再收尾。
				// @REASON: v0.2.110 工具调用修复——日志显示 fc 事件错位 + Codex 拿不到参数。
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
					if len(event.Data.Choices) > 0 {
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

	// @CODEX-DEBUG v0.2.98：翻译后下游请求形状
	log.Printf("[CODEX-DEBUG] TranslateToProvider: model=%q input_items=%d instructions_len=%d tools=%d stream=%v",
		req.Model, len(input), len(instructions), len(tools), req.Stream)

	// @AI_GUARD: RESPONSES_TOP_P_FILTER - SenseNova 要求 top_p ∈ (0, 1]
	var topP *float64
	if req.TopP != nil && *req.TopP > 0 {
		topP = req.TopP
	}

	return &ResponseRequest{
		Model:           req.Model,
		Input:           input,
		Tools:           tools,
		Stream:          req.Stream,
		Temperature:     req.Temperature,
		TopP:            topP,
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
// 图片 → {type:"input_image", image_url:"data:<mt>;base64,<data>"|"<url>"}
//
// image_url 是 Responses 协议规范（OpenAI 官方）的字段，Codex CLI 发的也是这个
// （codex-rs/protocol/src/models.rs ContentItem::InputImage.image_url）。此前这里写
// 的是 Anthropic 的 source:{type,data,media_type} 形状——那个形状 Anthropic 上游认，
// OpenAI 系上游不认，图片被静默丢弃。source 保留在结构体里是因为上游
// TranslateFromProvider 的 output_image 分支仍需要解析它。
func buildResponsesContentBlocks(blocks []schema.InternalContentBlock) []ContentBlock {
	var result []ContentBlock
	for _, cb := range blocks {
		switch cb.Type {
		case "text":
			result = append(result, ContentBlock{Type: "input_text", Text: cb.Text})
		case "image":
			url := responsesImageDataURL(cb)
			if url == "" {
				// 无数据无 URL 的空 image 块：写出去也没有信息量，跳过
				continue
			}
			result = append(result, ContentBlock{Type: "input_image", ImageURL: url})
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
		"error": streamErrorObject(err),
	})
	return errData
}

// streamErrorObject 返回 Responses 协议单层的 error 对象（不含外层 "error" 信封）。
// @AI_GUARD: RESPONSES_ERROR_SHAPE - 错误对象必须单层，禁止信封套信封
// @CONSTRAINT: TranslateError 产出 {"error":{...}} 信封供 writeData/writeStreamEvent
//
//	直接作为 SSE data 行；TranslateStream 的 error 分支要把它嵌进
//	response.completed.response.error，若复用信封就变成 {"error":{"error":{...}}}，
//	Codex 取 response.error.message 得到 undefined。
//
// @REASON: v0.2.119 — 上游 4xx 曾呈现为"成功但空内容"，且 error.message 是双重 JSON 编码
//
//	（错误体被 json.Marshal 成字符串再塞进 message 字段），客户端完全无法归因。
//	这里对 Message 是 JSON 对象时先解析还原，保持 message 为人类可读字符串。
func streamErrorObject(err *schema.StreamError) map[string]interface{} {
	obj := map[string]interface{}{}
	if err == nil {
		return obj
	}

	msg := strings.TrimSpace(err.Message)
	// 上游错误体常见为 JSON 对象；优先抽出其自带 message/error 字段
	if strings.HasPrefix(msg, "{") {
		var body map[string]any
		if json.Unmarshal([]byte(msg), &body) == nil {
			if m, ok := body["message"].(string); ok && m != "" {
				msg = m
			} else if m, ok := body["error"].(string); ok && m != "" {
				msg = m
			} else if e, ok := body["error"].(map[string]any); ok {
				if m2, ok2 := e["message"].(string); ok2 && m2 != "" {
					msg = m2
				}
			}
		}
	}
	if msg != "" {
		obj["message"] = msg
	} else {
		obj["message"] = "upstream error"
	}

	if err.Type != "" {
		obj["type"] = err.Type
	} else {
		obj["type"] = "invalid_request_error"
	}
	if err.Code != 0 {
		obj["code"] = err.Code
	}
	return obj
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

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"slices"
	"sync"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/middleware"
	"github.com/agent-proxy/agent-proxy/internal/protocol/anthropic"
	"github.com/agent-proxy/agent-proxy/internal/protocol/chatcompletion"
	"github.com/agent-proxy/agent-proxy/internal/protocol/gemini"
	"github.com/agent-proxy/agent-proxy/internal/protocol/responses"
	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
	"github.com/agent-proxy/agent-proxy/internal/provider"
	"github.com/agent-proxy/agent-proxy/internal/translator"
	"github.com/go-chi/chi/v5"
)

// QuickGateway 超简易模式：从 DB 选一条记录，支持 4×4 全协议互转
type QuickGateway struct {
	proxyName          string
	info               *schema.ProviderInfo
	timeout            int
	capabilities       []string
	translatorRegistry *translator.TranslatorRegistry
	// 透传上游 /v1/models 用的
	proxyBaseURL string // 上游 base URL（已去除末尾 /v1）
	proxyKey     string
	// 客户端认证
	clientKey        string
	clientKeyEnabled bool
	// 按协议类型按需创建的 provider
	providerCache sync.Map // string → provider.Provider
	// 详细日志级别（0=关闭 1=-v 2=-vv，仅快速模式有效）
	verboseLevel int
}

// NewQuickGateway 从 DB 记录创建一个超简易网关
// capabilities: 嗅探到的上游协议列表，如 ["openai", "anthropic", "gemini", "responses"]
func NewQuickGateway(name, baseURL, apiKey string, capabilities []string, timeout int, clientKey string, clientKeyEnabled bool, verboseLevel int) *QuickGateway {
	// 注册 4 个协议翻译器
	registry := translator.NewTranslatorRegistry()
	registry.Register(&chatcompletion.ChatCompletionTranslator{})
	registry.Register(anthropic.NewAnthropicTranslator("2023-06-01"))
	registry.Register(gemini.NewGeminiTranslator())
	registry.Register(responses.NewResponsesTranslator())

	return &QuickGateway{
		proxyName: name,
		info: &schema.ProviderInfo{
			Name:         name,
			BaseURL:      baseURL,
			APIToken:     apiKey,
			APIVersion:   "2023-06-01",
			Capabilities: capabilities,
		},
		timeout:            timeout,
		capabilities:       capabilities,
		translatorRegistry: registry,
		proxyBaseURL:       strings.TrimSuffix(baseURL, "/"),
		proxyKey:           apiKey,
		clientKey:          clientKey,
		clientKeyEnabled:   clientKeyEnabled,
		verboseLevel:       verboseLevel,
	}
}

// normalizeIngress 将入站协议名归一化为存储名
// detectIngressProtocol 返回 "chatcompletion" / "anthropic" / "gemini" / "responses"
// DB 中存储 "openai"（代表 Chat Completions），需要映射
func (q *QuickGateway) normalizeIngress(p string) string {
	if p == "chatcompletion" {
		return "openai"
	}
	return p
}

// selectProtocol 根据入站协议选择匹配的上游协议
// 策略：归一化后，若该协议在 capabilities 中则使用它（透传）；否则回退到 openai（翻译转换）
func (q *QuickGateway) selectProtocol(ingressProtocol string) string {
	normalized := q.normalizeIngress(ingressProtocol)
	if slices.Contains(q.capabilities, normalized) {
		return normalized
	}
	// 回退：上游不支持该协议 → 翻译到 openai 协议转换
	return "openai"
}

// getProvider 按需创建并缓存 provider（按协议类型）
func (q *QuickGateway) getProvider(protocolType string) provider.Provider {
	if cached, ok := q.providerCache.Load(protocolType); ok {
		return cached.(provider.Provider)
	}

	var p provider.Provider
	switch protocolType {
	case "anthropic":
		p = provider.NewAnthropicClient(q.proxyName, q.proxyBaseURL, q.proxyKey, "2023-06-01", q.timeout)
	case "gemini":
		p = provider.NewGeminiClient(q.proxyName, q.proxyBaseURL, q.timeout)
	case "responses":
		p = provider.NewResponsesClient(q.proxyName, q.proxyBaseURL, q.timeout)
	default:
		p = provider.NewOpenAIClient(q.proxyName, q.proxyBaseURL, q.timeout)
	}

	if val, loaded := q.providerCache.LoadOrStore(protocolType, p); loaded {
		return val.(provider.Provider)
	}
	return p
}

// verboseCtxKey 用于在 context 中传递 -v/-vv 日志所需的元信息
type verboseCtxKey struct{}

// verboseCtx 从 context 读取日志元信息
type verboseCtx struct {
	// 哪个客户端 IP 接入
	clientIP string
	// 入站协议（未归一化的原始协议名）
	ingressProtocol string
	// 上游协议（选择后的 provider 类型）
	providerType string
	// 模型名
	model string
	// 请求体（-vv 模式用）
	reqBody []byte
	// 上游 URL
	upstream string
}

func (q *QuickGateway) Routes() chi.Router {
	mux := chi.NewRouter()

	if q.clientKeyEnabled {
		mux.Use(middleware.Auth(q.clientKey))
	}

	mux.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"ok","mode":"quick","provider":"%s"}`, q.proxyName)
	})

	mux.Get("/v1/models", q.handleModels)

	// 4 个协议端点全部走统一的中枢翻译
	mux.Post("/v1/chat/completions", q.handleChatCompletion)
	mux.Post("/v1/messages", q.handleMessages)
	mux.Post("/v1/responses", q.handleResponses)

	// Gemini: /v1/models/{model}:generateContent — chi * wildcard for colon separator
	mux.Post("/v1/models/*", q.handleModelsCatchAll)

	return mux
}

// handleRequest 统一请求处理：入站协议 → InternalRequest → 下游 Provider → InternalResponse → 入站协议
// 协议感知路由：先按入站协议选择匹配的上游协议（透传优先），无匹配则回退到 openai 翻译转换。
func (q *QuickGateway) handleRequest(w http.ResponseWriter, r *http.Request, ingressProtocol string) {
	startTime := time.Now()

	// 读取请求体
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		q.sendError(w, http.StatusBadRequest, "read_body", err.Error())
		return
	}
	// 重新包装 r.Body，让下游处理函数能再次读取
	br := bytes.NewReader(body)
	r.Body = io.NopCloser(io.NewSectionReader(br, 0, int64(br.Len())))

	// ── 协议感知路由：选择上游协议（本地变量，不修改 q.info 共享结构） ──
	providerType := q.selectProtocol(ingressProtocol)
	p := q.getProvider(providerType)

	// ── 轻量提取 model 用于路由 & 透传 URL ──
	// Gemini 的 model 在 URL 路径 /v1/models/{model}:generateContent 中，
	// 已由 handleModelsCatchAll 写入 ctx；其他协议从请求体读取。
	model := quickExtractModel(body)
	if model == "" && ingressProtocol == "gemini" {
		if m, ok := gemini.GeminiModelFromContext(r.Context()); ok && m != "" {
			model = m
		}
	}

	// ── 透传 vs 翻译 ──
	// 归一化后 ingressProtocol == providerType 时透传（零损耗）
	normalizedIngress := q.normalizeIngress(ingressProtocol)

	// 提取客户端 IP（剥离端口）
	clientIP := r.RemoteAddr
	if idx := strings.LastIndex(clientIP, ":"); idx > 0 {
		clientIP = clientIP[:idx]
	}

	// 构建 verbose 日志上下文（用于 -v/-vv 输出）
	vctx := verboseCtx{
		clientIP:        clientIP,
		ingressProtocol: ingressProtocol,
		providerType:    providerType,
		model:           model,
		reqBody:         body,
		upstream:        q.proxyBaseURL,
	}

	if normalizedIngress == providerType && model != "" {
		ctx := context.WithValue(r.Context(), verboseCtxKey{}, vctx)
		if quickDetectStream(body) {
			q.handlePassthroughStream(p, ctx, w, r, model, startTime)
		} else {
			q.handlePassthroughNonStream(p, ctx, w, r, model, startTime)
		}
		return
	}

	// ── 入站翻译：协议请求 → InternalRequest ──
	ingressTranslator := q.translatorRegistry.Get(ingressProtocol)
	if ingressTranslator == nil {
		q.sendError(w, http.StatusInternalServerError, "unknown_protocol", ingressProtocol)
		return
	}

	internalReq, err := ingressTranslator.TranslateRequest(r.Context(), body)
	if err != nil {
		q.sendError(w, http.StatusBadRequest, "translate_request", err.Error())
		return
	}

	// ── 翻译到目标 Provider 协议 ──
	providerTranslator, downstreamReq := q.translateToProvider(providerType, internalReq)

	// ── 执行 Provider 调用 ──
	stream := internalReq.Stream
	ctx := context.WithValue(r.Context(), verboseCtxKey{}, vctx)
	if stream {
		q.handleStreamRequest(p, ctx, w, downstreamReq, providerTranslator, ingressTranslator, startTime)
	} else {
		q.handleNonStreamResponse(p, ctx, w, downstreamReq, providerTranslator, ingressTranslator, internalReq, startTime)
	}
}

func (q *QuickGateway) handleChatCompletion(w http.ResponseWriter, r *http.Request) {
	q.handleRequest(w, r, "chatcompletion")
}

func (q *QuickGateway) handleMessages(w http.ResponseWriter, r *http.Request) {
	q.handleRequest(w, r, "anthropic")
}

func (q *QuickGateway) handleResponses(w http.ResponseWriter, r *http.Request) {
	q.handleRequest(w, r, "responses")
}

func (q *QuickGateway) handleGenerateContent(w http.ResponseWriter, r *http.Request) {
	q.handleRequest(w, r, "gemini")
}

// handleModelsCatchAll 通配 /v1/models/*，处理冒号分隔的 Gemini 路径
// 格式：/v1/models/{model}:generateContent
func (q *QuickGateway) handleModelsCatchAll(w http.ResponseWriter, r *http.Request) {
	suffix := chi.URLParam(r, "*")
	if !strings.HasSuffix(suffix, ":generateContent") {
		http.NotFound(w, r)
		return
	}
	model := strings.TrimSuffix(suffix, ":generateContent")
	model = strings.TrimPrefix(model, "/")
	ctx := gemini.WithGeminiModel(r.Context(), model)
	q.handleGenerateContent(w, r.WithContext(ctx))
}

// quickExtractModel 从原始 JSON 中提取 model 字段
func quickExtractModel(body json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	if v, ok := m["model"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// quickDetectStream 检测请求体中 stream 字段
func quickDetectStream(body json.RawMessage) bool {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	if v, ok := m["stream"]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// makeQuickPassthroughInfo 为 Quick 透传构造 ProviderInfo：把 model 写入 Name
//（各 provider BuildURL 的 model 参数来自 info.Name），并带上 APIToken 用于认证。
func makeQuickPassthroughInfo(info *schema.ProviderInfo, model string) *schema.ProviderInfo {
	return &schema.ProviderInfo{
		Name:     model,
		BaseURL:  info.BaseURL,
		APIToken: info.APIToken,
	}
}

// handlePassthroughNonStream 透传非流式：请求/响应都不翻译，原样转发
func (q *QuickGateway) handlePassthroughNonStream(p provider.Provider, ctx context.Context, w http.ResponseWriter,
	r *http.Request, model string, startTime time.Time) {

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(q.timeout)*time.Second)
	defer cancel()

	callInfo := makeQuickPassthroughInfo(q.info, model)
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		q.sendError(w, http.StatusBadRequest, "read_body", err.Error())
		return
	}

	resp, headers, err := p.Call(callCtx, body, callInfo)
	if err != nil {
		if httpStatus, bodyData := parseCallError(err); httpStatus > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(httpStatus)
			w.Write([]byte(bodyData))
			return
		}
		q.sendError(w, http.StatusInternalServerError, "provider_error", err.Error())
		return
	}

	// 透传下游响应头
	for k, v := range headers {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(resp)

	usage := q.extractUsage(resp)
	vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
	q.logRequest(vctx, startTime, http.StatusOK, usage, resp)
}

// handlePassthroughStream 透传流式：下游 SSE 过滤元数据后原样转发
func (q *QuickGateway) handlePassthroughStream(p provider.Provider, ctx context.Context, w http.ResponseWriter,
	r *http.Request, model string, startTime time.Time) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		q.sendError(w, http.StatusInternalServerError, "streaming not supported", "server does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(q.timeout)*time.Second)
	defer cancel()

	callInfo := makeQuickPassthroughInfo(q.info, model)
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		flusher.Flush()
		q.sendError(w, http.StatusBadRequest, "read_body", err.Error())
		return
	}

	lines, headers, err := p.CallStream(callCtx, body, callInfo)
	if err != nil {
		flusher.Flush()
		q.sendError(w, http.StatusInternalServerError, "stream_error", err.Error())
		return
	}

	// 透传下游响应头
	for k, v := range headers {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}

	var lastUsage *schema.InternalUsage
	for line := range lines {
		var meta map[string]any
		if json.Unmarshal(line, &meta) == nil {
			if meta["_type"] == "headers" {
				continue
			}
			if meta["_type"] == "error" {
				status, _ := meta["_status"].(float64)
				if status == 0 {
					status = 502
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(int(status))
				if data, ok := meta["data"].(string); ok {
					w.Write([]byte(data))
				}
				return
			}
		}
		// 累积 usage（用于 -v 日志）
		usage := q.extractUsage(line)
		if usage != nil {
			lastUsage = usage
		}
		w.Write(line)
		w.Write([]byte("\n"))
		flusher.Flush()
	}

	vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
	q.logRequest(vctx, startTime, http.StatusOK, lastUsage, nil)
}

// handleNonStreamResponse 非流式响应
func (q *QuickGateway) handleNonStreamResponse(p provider.Provider, ctx context.Context, w http.ResponseWriter,
	downstreamReq json.RawMessage, providerTranslator any,
	ingressTranslator translator.CombinedTranslator, internalReq *schema.InternalRequest,
	startTime time.Time) {

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(q.timeout)*time.Second)
	defer cancel()

	resp, headers, err := p.Call(callCtx, downstreamReq, q.info)
	if err != nil {
		if httpStatus, bodyData := parseCallError(err); httpStatus > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(httpStatus)
			w.Write([]byte(bodyData))
			return
		}
		q.sendError(w, http.StatusInternalServerError, "provider_error", err.Error())
		return
	}

	// ── 出 Provider 翻译：Provider 响应 → InternalResponse ──
	var internalResp *schema.InternalResponse
	if providerTranslator != nil {
		pt := providerTranslator.(interface {
			TranslateFromProvider(json.RawMessage) (*schema.InternalResponse, error)
		})
		internalResp, err = pt.TranslateFromProvider(resp)
		if err != nil {
			q.sendError(w, http.StatusInternalServerError, "translate_response", err.Error())
			return
		}
	} else {
		// OpenAI 兼容：直接解析为 InternalResponse
		var ccResp chatcompletion.ChatCompletionResponse
		if err := json.Unmarshal(resp, &ccResp); err != nil {
			q.sendError(w, http.StatusInternalServerError, "parse_response", err.Error())
			return
		}
		internalResp = chatCompletionToInternal(&ccResp)
	}

	// ── 出站翻译：InternalResponse → 入站协议格式 ──
	outgoingResp, err := ingressTranslator.TranslateResponse(internalResp)
	if err != nil {
		q.sendError(w, http.StatusInternalServerError, "encode_response", err.Error())
		return
	}

	// 透传下游响应头
	for k, v := range headers {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(outgoingResp)

	usage := internalResp.Usage
	vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
	q.logRequest(vctx, startTime, http.StatusOK, usage, outgoingResp)
}

// handleStreamRequest 流式请求
func (q *QuickGateway) handleStreamRequest(p provider.Provider, ctx context.Context, w http.ResponseWriter,
	downstreamReq json.RawMessage, providerTranslator any,
	ingressTranslator translator.CombinedTranslator,
	startTime time.Time) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		q.sendError(w, http.StatusInternalServerError, "stream", "server does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(q.timeout)*time.Second)
	defer cancel()

	lines, _, err := p.CallStream(callCtx, downstreamReq, q.info)
	if err != nil {
		flusher.Flush()
		q.sendError(w, http.StatusInternalServerError, "stream_error", err.Error())
		return
	}

	// 构建内部流式事件 channel
	events := make(chan schema.InternalStreamEvent, 16)
	var accumulatedUsage *schema.InternalUsage
	go func() {
		defer close(events)
		for line := range lines {
			// 跳过元数据
			var meta map[string]any
			if json.Unmarshal(line, &meta) == nil {
				if meta["_type"] == "headers" {
					continue
				}
				if meta["_type"] == "error" {
				status, _ := meta["_status"].(float64)
				if status == 0 {
					status = 502
				}
				data, _ := meta["data"].(string)
				if data == "" {
					data = fmt.Sprintf("upstream error: HTTP %.0f", status)
				}
				events <- schema.InternalStreamEvent{
					Type: "error",
					Error: &schema.StreamError{
						Message: data,
						Type:    "upstream_error",
						Code:    int(status),
					},
				}
				return
			}
			}

			// Provider 翻译器解析流式事件
			if providerTranslator != nil {
				pte := providerTranslator.(interface {
					TranslateStreamEvent(json.RawMessage) *schema.InternalStreamEvent
				})
				event := pte.TranslateStreamEvent(line)
				if event != nil {
					if event.Data != nil && event.Data.Usage != nil {
						accumulatedUsage = event.Data.Usage
					}
					events <- *event
				}
			} else {
				// OpenAI 兼容：解析 SSE delta 行（可能带 "data: " 前缀）
				payload := strings.TrimPrefix(string(line), "data: ")
				var ccChunk chatcompletion.ChatCompletionStreamChunk
				if json.Unmarshal([]byte(payload), &ccChunk) != nil || len(ccChunk.Choices) == 0 {
					continue
				}
				choice := ccChunk.Choices[0]
				msg := schema.InternalMessage{Role: schema.RoleAssistant}
				text := choice.Delta.Content
				if text == "" {
					text = choice.Delta.Reasoning
				}
				if text != "" {
					msg.Content, _ = json.Marshal(text)
				}
				for _, tc := range choice.Delta.ToolCalls {
					msg.ToolCalls = append(msg.ToolCalls, schema.InternalToolCall{
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
				eventType := "delta"
				if choice.FinishReason != "" {
					eventType = "done"
				}
				events <- schema.InternalStreamEvent{
					Type: eventType,
					Data: &schema.InternalStreamChunk{
						ID:    ccChunk.ID,
						Model: ccChunk.Model,
						Choices: []schema.InternalChoice{{
							Index:        choice.Index,
							Message:      msg,
							FinishReason: choice.FinishReason,
						}},
						Usage: mapInternalUsage(ccChunk.Usage),
					},
				}
				if ccChunk.Usage != nil {
					accumulatedUsage = mapInternalUsage(ccChunk.Usage)
				}
			}
		}
	}()

	// 使用入站翻译器的 TranslateStream 写出站 SSE
	ingressTranslator.TranslateStream(ctx, events, func(eventData []byte, isDone bool) {
		w.Write(eventData)
		if !isDone {
			flusher.Flush()
		}
	})

	vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
	q.logRequest(vctx, startTime, http.StatusOK, accumulatedUsage, nil)
}

// translateToProvider 根据目标 Provider 类型选择翻译器并构建下游请求体
// providerType 来自调用方本地变量，避免读取共享 q.info.Version 造成数据竞争。
func (q *QuickGateway) translateToProvider(providerType string, internalReq *schema.InternalRequest) (any, json.RawMessage) {

	switch providerType {
	case "openai", "sensenova":
		// OpenAI 兼容：使用 InternalRequest 的 Model
		ccReq := buildCCRequest(internalReq)
		downstreamReq, _ := json.Marshal(ccReq)
		return nil, downstreamReq

	case "anthropic":
		pt := anthropic.NewAnthropicTranslator(q.info.APIVersion)
		req, _ := pt.TranslateToProvider(internalReq)
		downstreamReq, _ := json.Marshal(req)
		return pt, downstreamReq

	case "gemini":
		pt := gemini.NewGeminiTranslator()
		req, _ := pt.TranslateToProvider(internalReq)
		downstreamReq, _ := json.Marshal(req)
		return pt, downstreamReq

	case "responses":
		pt := responses.NewResponsesTranslator()
		req, _ := pt.TranslateToProvider(internalReq)
		downstreamReq, _ := json.Marshal(req)
		return pt, downstreamReq

	default:
		downstreamReq, _ := json.Marshal(buildCCRequest(internalReq))
		return nil, downstreamReq
	}
}

func (q *QuickGateway) sendError(w http.ResponseWriter, code int, typ, msg string) {
	errResp := map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    typ,
			"code":    fmt.Sprintf("%d", code),
		},
	}
	data, _ := json.Marshal(errResp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(data)
}

// handleModels 透传上游 /v1/models，实时获取模型列表
func (q *QuickGateway) handleModels(w http.ResponseWriter, r *http.Request) {
	modelsURL := q.proxyBaseURL + "/v1/models"
	req, err := http.NewRequest("GET", modelsURL, nil)
	if err != nil {
		q.sendError(w, http.StatusInternalServerError, "proxy_error", err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+q.proxyKey)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		q.sendError(w, http.StatusBadGateway, "upstream_unreachable", err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		q.sendError(w, resp.StatusCode, "upstream_error", string(body))
		return
	}

	// 透传上游响应头
	for k, v := range resp.Header {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// parseCallError 解析 provider.Call 返回的错误。
// provider.Call 在 HTTP >= 400 时返回 "HTTP <code>: <body>" 格式的错误，
// 解析后返回 HTTP 状态码和原始响应体，以便服务端透传上游错误。
// 无法解析时返回 (0, "")。
func parseCallError(err error) (int, string) {
	msg := err.Error()
	const prefix = "HTTP "
	idx := strings.Index(msg, prefix)
	if idx < 0 {
		return 0, ""
	}
	rest := msg[idx+len(prefix):]
	// 找到冒号分隔符
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return 0, ""
	}
	var code int
	if _, nErr := fmt.Sscanf(rest[:colon], "%d", &code); nErr != nil || code < 400 || code > 599 {
		return 0, ""
	}
	body := strings.TrimSpace(rest[colon+1:])
	return code, body
}

// logRequest 输出 -v / -vv 级别的请求日志。
// -v 级别: 显示客户端 IP、入站协议、上游协议、模型、状态码、token 用量
// -vv 级别: 在 -v 基础上额外显示 Guest 侧请求体和 LLM 侧响应内容
func (q *QuickGateway) logRequest(vctx verboseCtx, startTime time.Time, status int, usage *schema.InternalUsage, respBody []byte) {
	if q.verboseLevel == 0 {
		return
	}
	if vctx.ingressProtocol == "" {
		return
	}

	latency := time.Since(startTime).Milliseconds()
	ingressName := normalizeProtocolName(vctx.ingressProtocol)

	if q.verboseLevel >= 1 {
		usageStr := "—"
		if usage != nil {
			usageStr = fmt.Sprintf("prompt=%d, completion=%d, total=%d",
				usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
		}
		fmt.Printf("[请求 %s] 上游: %s  |  协议: %s → %s  |  模型: %s  |  状态: %d  |  耗时: %dms  |  Token: %s\n",
			vctx.clientIP, vctx.upstream, ingressName, vctx.providerType, vctx.model, status, latency, usageStr)
	}

	if q.verboseLevel >= 2 {
		if len(vctx.reqBody) > 0 {
			fmt.Printf("[Guest → 代理] 请求体:\n%s\n", truncateJSON(vctx.reqBody, 600))
		}
		if len(respBody) > 0 {
			fmt.Printf("[代理 → LLM] 响应体:\n%s\n", truncateJSON(respBody, 800))
		}
	}
}

// extractUsage 从原始 JSON 响应中提取 token 用量。
// 支持多种格式:
//   - OpenAI: usage.prompt_tokens / completion_tokens / total_tokens
//   - Anthropic: usage.input_tokens / output_tokens
//   - Responses API: usage.input_tokens / output_tokens
func (q *QuickGateway) extractUsage(resp []byte) *schema.InternalUsage {
	var m map[string]any
	if err := json.Unmarshal(resp, &m); err != nil {
		return nil
	}
	usageMap, ok := m["usage"].(map[string]any)
	if !ok {
		return nil
	}

	usage := &schema.InternalUsage{}
	if v, ok := usageMap["prompt_tokens"].(float64); ok {
		usage.PromptTokens = int(v)
	}
	if v, ok := usageMap["completion_tokens"].(float64); ok {
		usage.CompletionTokens = int(v)
	}
	// Anthropic/Responses 用 input_tokens / output_tokens
	if v, ok := usageMap["input_tokens"].(float64); ok {
		usage.PromptTokens = int(v)
	}
	if v, ok := usageMap["output_tokens"].(float64); ok {
		usage.CompletionTokens = int(v)
	}
	if v, ok := usageMap["total_tokens"].(float64); ok {
		usage.TotalTokens = int(v)
	}
	if v, ok := usageMap["cache_creation_input_tokens"].(float64); ok {
		usage.CacheCreationTokens = int(v)
	}
	if v, ok := usageMap["cache_read_input_tokens"].(float64); ok {
		usage.CacheReadTokens = int(v)
	}
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0 {
		return nil
	}
	return usage
}

// normalizeProtocolName 将协议内部名映射为用户可读名
func normalizeProtocolName(proto string) string {
	switch proto {
	case "openai", "chatcompletion":
		return "OpenAI"
	case "anthropic", "messages":
		return "Anthropic"
	case "gemini":
		return "Gemini"
	case "responses":
		return "Responses"
	default:
		return proto
	}
}

// truncateJSON 将 JSON 字节切片格式化为缩进 JSON 并截断到 maxLen 字节
func truncateJSON(raw []byte, maxLen int) string {
	var data any
	if json.Unmarshal(raw, &data) != nil {
		return truncateString(string(raw), maxLen)
	}
	pretty, _ := json.MarshalIndent(data, "", "  ")
	return truncateString(string(pretty), maxLen)
}

// truncateString 截断字符串到 maxLen，并附加省略标记
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "...\n  (truncated, " + fmt.Sprintf("%d bytes total", len(s)) + ")"
}

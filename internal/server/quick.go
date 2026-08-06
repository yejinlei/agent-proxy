package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	provider           provider.Provider
	translatorRegistry *translator.TranslatorRegistry
	// 透传上游 /v1/models 用的
	proxyBaseURL string // 上游 base URL（已去除末尾 /v1）
	proxyKey     string
	// 客户端认证
	clientKey          string
	clientKeyEnabled   bool
}

// NewQuickGateway 从 DB 记录创建一个超简易网关
func NewQuickGateway(name, baseURL, apiKey, providerType string, timeout int, clientKey string, clientKeyEnabled bool) *QuickGateway {
	var p provider.Provider
	switch providerType {
	case "anthropic":
		p = provider.NewAnthropicClient(name, baseURL, apiKey, "2023-06-01", timeout)
	case "gemini":
		p = provider.NewGeminiClient(name, baseURL, timeout)
	case "responses":
		p = provider.NewResponsesClient(name, baseURL, timeout)
	default:
		p = provider.NewOpenAIClient(name, baseURL, timeout)
	}

	// 注册 4 个协议翻译器
	registry := translator.NewTranslatorRegistry()
	registry.Register(&chatcompletion.ChatCompletionTranslator{})
	registry.Register(anthropic.NewAnthropicTranslator("2023-06-01"))
	registry.Register(gemini.NewGeminiTranslator())
	registry.Register(responses.NewResponsesTranslator())

	return &QuickGateway{
		proxyName: name,
		info: &schema.ProviderInfo{
			Name:       name,
			BaseURL:    baseURL,
			APIToken:   apiKey,
			Version:    providerType,
			APIVersion: "2023-06-01",
		},
		provider:           p,
		translatorRegistry: registry,
		proxyBaseURL:       strings.TrimSuffix(baseURL, "/"),
		proxyKey:           apiKey,
		clientKey:          clientKey,
		clientKeyEnabled:   clientKeyEnabled,
	}
}

// detectIngressProtocol 从请求路径识别入站协议
func (q *QuickGateway) detectIngressProtocol(path string) string {
	switch {
	case path == "/v1/chat/completions":
		return "chatcompletion"
	case path == "/v1/messages":
		return "anthropic"
	case path == "/v1/responses":
		return "responses"
	case strings.HasSuffix(path, ":generateContent"):
		return "gemini"
	default:
		return "chatcompletion"
	}
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
// 优先透传：当入站协议与 Provider 类型一致时，原始 body 原样转发，零损耗。
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

	// ── 轻量提取 model 用于路由 & 透传 URL ──
	// Gemini 的 model 在 URL 路径 /v1/models/{model}:generateContent 中，
	// 已由 handleModelsCatchAll 写入 ctx；其他协议从请求体读取。
	model := quickExtractModel(body, ingressProtocol)
	if model == "" && ingressProtocol == "gemini" {
		if m, ok := gemini.GeminiModelFromContext(r.Context()); ok && m != "" {
			model = m
		}
	}

	// ── 透传 vs 翻译 ──
	providerType := q.info.Version
	if ingressProtocol == providerType && model != "" {
		ctx := r.Context()
		if quickDetectStream(body) {
			q.handlePassthroughStream(ctx, w, r, model, startTime)
		} else {
			q.handlePassthroughNonStream(ctx, w, r, model, startTime)
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
	providerTranslator, downstreamReq := q.translateToProvider(internalReq)

	// ── 执行 Provider 调用 ──
	stream := internalReq.Stream
	ctx := r.Context()
	if stream {
		q.handleStreamRequest(ctx, w, downstreamReq, providerTranslator, ingressTranslator, startTime, r)
	} else {
		q.handleNonStreamResponse(ctx, w, downstreamReq, providerTranslator, ingressTranslator, internalReq, startTime, r)
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
func quickExtractModel(body json.RawMessage, ingressProtocol string) string {
	var m map[string]interface{}
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
	var m map[string]interface{}
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
func (q *QuickGateway) handlePassthroughNonStream(ctx context.Context, w http.ResponseWriter,
	r *http.Request, model string, startTime time.Time) {

	callCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	callInfo := makeQuickPassthroughInfo(q.info, model)
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		q.sendError(w, http.StatusBadRequest, "read_body", err.Error())
		return
	}

	resp, headers, err := q.provider.Call(callCtx, body, callInfo)
	if err != nil {
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

	latency := time.Since(startTime).Milliseconds()
	fmt.Printf("[%s] passthrough %s %dms\n", time.Now().Format("15:04:05"), q.proxyName, latency)
}

// handlePassthroughStream 透传流式：下游 SSE 过滤元数据后原样转发
func (q *QuickGateway) handlePassthroughStream(ctx context.Context, w http.ResponseWriter,
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

	callCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	callInfo := makeQuickPassthroughInfo(q.info, model)
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		flusher.Flush()
		q.sendError(w, http.StatusBadRequest, "read_body", err.Error())
		return
	}

	lines, headers, err := q.provider.CallStream(callCtx, body, callInfo)
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

	for line := range lines {
		var meta map[string]interface{}
		if json.Unmarshal(line, &meta) == nil && meta["_type"] == "headers" {
			continue
		}
		w.Write(line)
		w.Write([]byte("\n"))
		flusher.Flush()
	}

	latency := time.Since(startTime).Milliseconds()
	fmt.Printf("[%s] passthrough stream %s %dms\n", time.Now().Format("15:04:05"), q.proxyName, latency)
}

// handleNonStreamResponse 非流式响应
func (q *QuickGateway) handleNonStreamResponse(ctx context.Context, w http.ResponseWriter,
	downstreamReq json.RawMessage, providerTranslator interface{},
	ingressTranslator translator.CombinedTranslator, internalReq *schema.InternalRequest,
	startTime time.Time, r *http.Request) {

	callCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	resp, headers, err := q.provider.Call(callCtx, downstreamReq, q.info)
	if err != nil {
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

	latency := time.Since(startTime).Milliseconds()
	fmt.Printf("[%s] %s → %s %dms\n", time.Now().Format("15:04:05"), internalReq.Model, q.proxyName, latency)
}

// handleStreamRequest 流式请求
func (q *QuickGateway) handleStreamRequest(ctx context.Context, w http.ResponseWriter,
	downstreamReq json.RawMessage, providerTranslator interface{},
	ingressTranslator translator.CombinedTranslator,
	startTime time.Time, r *http.Request) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		q.sendError(w, http.StatusInternalServerError, "stream", "server does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	callCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	lines, _, err := q.provider.CallStream(callCtx, downstreamReq, q.info)
	if err != nil {
		flusher.Flush()
		q.sendError(w, http.StatusInternalServerError, "stream_error", err.Error())
		return
	}

	// 构建内部流式事件 channel
	events := make(chan schema.InternalStreamEvent, 16)
	go func() {
		defer close(events)
		for line := range lines {
			// 跳过元数据
			var meta map[string]interface{}
			if json.Unmarshal(line, &meta) == nil && meta["_type"] == "headers" {
				continue
			}

			// Provider 翻译器解析流式事件
			if providerTranslator != nil {
				pte := providerTranslator.(interface {
					TranslateStreamEvent(json.RawMessage) *schema.InternalStreamEvent
				})
				event := pte.TranslateStreamEvent(line)
				if event != nil {
					events <- *event
				}
			} else {
				// OpenAI 兼容：解析 SSE delta 行（可能带 "data: " 前缀）
				payload := string(line)
				if strings.HasPrefix(payload, "data: ") {
					payload = payload[6:]
				}
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
				events <- schema.InternalStreamEvent{
					Type: "delta",
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

	latency := time.Since(startTime).Milliseconds()
	fmt.Printf("[%s] stream %s %dms\n", time.Now().Format("15:04:05"), q.proxyName, latency)
}

// translateToProvider 根据目标 Provider 类型选择翻译器并构建下游请求体
func (q *QuickGateway) translateToProvider(internalReq *schema.InternalRequest) (interface{}, json.RawMessage) {
	providerType := q.info.Version

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
	errResp := map[string]interface{}{
		"error": map[string]interface{}{
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

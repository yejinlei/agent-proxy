package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/config"
	"github.com/agent-proxy/agent-proxy/internal/db"
	"github.com/agent-proxy/agent-proxy/internal/middleware"
	"github.com/agent-proxy/agent-proxy/internal/monitor"
	"github.com/agent-proxy/agent-proxy/internal/protocol/anthropic"
	"github.com/agent-proxy/agent-proxy/internal/protocol/chatcompletion"
	"github.com/agent-proxy/agent-proxy/internal/protocol/gemini"
	"github.com/agent-proxy/agent-proxy/internal/protocol/responses"
	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
	"github.com/agent-proxy/agent-proxy/internal/provider"
	"github.com/agent-proxy/agent-proxy/internal/router"
	"github.com/agent-proxy/agent-proxy/internal/translator"
	"github.com/agent-proxy/agent-proxy/internal/web"

	"github.com/go-chi/chi/v5"
)

// ═══════════════════════════════════════════════════════════════════════════════
//  协议矩阵：4×4 全协议互转
//
//  入站协议（Ingress Protocol）：由请求路径决定
//    POST /v1/chat/completions  → chatcompletion
//    POST /v1/messages           → anthropic
//    POST /v1/responses          → responses
//    POST /v1/models/{model}:generateContent → gemini
//
//  出站协议（Egress Protocol）：由目标 Provider 类型决定
//    openai/sensenova → OpenAI Compatible
//    anthropic        → Anthropic Messages
//    gemini           → Google Gemini
//    responses        → OpenAI Responses
//
//  翻译链路：
//    入站协议 TranslateRequest → InternalRequest → 路由 Provider
//    → TranslateToProvider → Provider 调用
//    → TranslateFromProvider → InternalResponse
//    → 入站协议 TranslateResponse → 出站响应
// ═══════════════════════════════════════════════════════════════════════════════

// Gateway HTTP 网关服务
type Gateway struct {
	cfg                *config.Config
	registry           *provider.ProviderRegistry
	router             *router.ModelRouter
	translatorRegistry *translator.TranslatorRegistry
	store              *monitor.Store
	webServer          *web.Server
	rateLimiter        *rateLimiter
	aliasFile          *db.AliasFile
}

func NewGateway(cfg *config.Config) *Gateway {
	store := monitor.NewStore(cfg.Monitor.LogSize)

	// 注册 4 个协议的完整翻译器
	translatorRegistry := translator.NewTranslatorRegistry()
	translatorRegistry.Register(&chatcompletion.ChatCompletionTranslator{})
	translatorRegistry.Register(anthropic.NewAnthropicTranslator("2023-06-01"))
	translatorRegistry.Register(gemini.NewGeminiTranslator())
	translatorRegistry.Register(responses.NewResponsesTranslator())

	// 初始化 Provider 客户端
	providerRegistry := provider.NewProviderRegistry()
	for name, pc := range cfg.Providers {
		switch pc.ProviderType {
		case "openai", "sensenova":
			providerRegistry.Register(provider.NewOpenAIClient(name, pc.BaseURL, pc.TimeoutSec))
		case "anthropic":
			providerRegistry.Register(provider.NewAnthropicClient(name, pc.BaseURL, pc.APIToken, pc.APIVersion, pc.TimeoutSec))
		case "gemini":
			providerRegistry.Register(provider.NewGeminiClient(name, pc.BaseURL, pc.TimeoutSec))
		case "responses":
			providerRegistry.Register(provider.NewResponsesClient(name, pc.BaseURL, pc.TimeoutSec))
		}
	}

	// 路由器
	modelRouter := router.NewModelRouter()
	modelRouter.SetDefaultProvider(cfg.ModelRouter.DefaultProvider)
	modelRouter.SetPrefixMatch(cfg.ModelRouter.PrefixMatch)
	for model, providerName := range cfg.ModelRouter.ModelToProvider {
		modelRouter.AddRoute(model, providerName)
	}
	for name, pc := range cfg.Providers {
		modelRouter.AddProvider(name, pc.ToProviderInfo())
	}

	webServer := web.NewServer(store, cfg.Monitor.UIPath)

	return &Gateway{
		cfg:                cfg,
		registry:           providerRegistry,
		router:             modelRouter,
		translatorRegistry: translatorRegistry,
		store:              store,
		webServer:          webServer,
		rateLimiter:        newRateLimiter(cfg.RateLimit.RequestsPerSecond, cfg.RateLimit.Burst),
	}
}

// detectIngressProtocol 从请求路径识别入站协议
func (g *Gateway) detectIngressProtocol(path string) string {
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

func (g *Gateway) Routes() chi.Router {
	mux := chi.NewRouter()

	mux.Use(middleware.Logger())
	mux.Use(middleware.Recoverer)
	mux.Use(middleware.CORS)

	if g.cfg.Auth.APIKey != "" {
		mux.Use(middleware.Auth(g.cfg.Auth.APIKey))
	}

	// 4 个协议端点全部走统一的中枢翻译
	mux.Post("/v1/chat/completions", g.handleChatCompletion)
	mux.Post("/v1/messages", g.handleMessages)
	mux.Post("/v1/responses", g.handleResponses)
	mux.Post("/v1/models", g.handleModels)

	// Gemini: /v1/models/{model}:generateContent
	// chi * wildcard to handle the colon separator, dispatch inside handler
	mux.Post("/v1/models/*", g.handleModelsCatchAll)

	mux.Get("/health", g.handleHealth)
	mux.Get("/status", g.handleStatus)

	if g.cfg.Monitor.Enabled {
		mux.Mount(g.cfg.Monitor.UIPath, g.webServer.Handle())
	}

	return mux
}

// handleRequest 统一请求处理：入站协议 → InternalRequest → 路由 → Provider → InternalResponse → 入站协议
func (g *Gateway) handleRequest(w http.ResponseWriter, r *http.Request, ingressProtocol string) {
	startTime := time.Now()
	g.store.IncrActiveConns()
	defer g.store.DecrActiveConns()

	// 读取请求体
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		g.sendError(w, ingressProtocol, http.StatusBadRequest, "invalid JSON", err.Error())
		return
	}

	// 速率限制
	if g.cfg.RateLimit.Enabled {
		if !g.rateLimiter.Allow() {
			g.sendError(w, ingressProtocol, http.StatusTooManyRequests, "rate limited", "exceeded rate limit")
			return
		}
	}

	// ── 轻量提取 model 用于路由（不解构整个请求体） ──
	// Gemini 的 model 在 URL 路径 /v1/models/{model}:generateContent 中，已从 catch-all
	// 写入 ctx，通过 GeminiModelFromContext 读取；其他协议从请求体读取。
	model := extractModelFromRaw(body, ingressProtocol)
	if model == "" && ingressProtocol == "gemini" {
		if m, ok := gemini.GeminiModelFromContext(r.Context()); ok && m != "" {
			model = m
		}
	}
	if model == "" {
		g.sendError(w, ingressProtocol, http.StatusBadRequest, "missing model", "model is required")
		return
	}

	// ── 模型别名解析（优先于 ModelRouter） ──
	realModel, originalModel, aliasHit := g.resolveAlias(model)

	// ── 路由 Provider ──
	info, providerName, err := g.router.Resolve(realModel)
	if err != nil {
		g.sendError(w, ingressProtocol, http.StatusNotFound, "model not found", err.Error())
		return
	}

	providerClient := g.registry.Get(providerName)
	if providerClient == nil {
		g.sendError(w, ingressProtocol, http.StatusInternalServerError, "provider not found", providerName)
		return
	}

	// 轻量检测 stream 字段
	stream := detectStream(body)

	// ── 透传 vs 翻译 ──
	// ingress 协议 == provider 协议时，直接透传原始 body，零损耗
	providerType := info.Version
	if ingressProtocol == providerType {
		ctx := r.Context()
		if stream {
			g.handlePassthroughStream(ctx, w, r, providerClient, info, body, realModel, ingressProtocol, startTime)
		} else {
			g.handlePassthroughNonStream(ctx, w, r, providerClient, info, body, realModel, originalModel, aliasHit, ingressProtocol, startTime)
		}
		return
	}

	// ── 入站翻译：协议请求 → InternalRequest ──
	ingressTranslator := g.translatorRegistry.Get(ingressProtocol)
	if ingressTranslator == nil {
		g.sendError(w, ingressProtocol, http.StatusInternalServerError, "unknown protocol", ingressProtocol)
		return
	}

	internalReq, err := ingressTranslator.TranslateRequest(r.Context(), body)
	if err != nil {
		g.sendError(w, ingressProtocol, http.StatusBadRequest, "translate request failed", err.Error())
		return
	}

	// 设置模型名和别名回显
	internalReq.Model = realModel
	internalReq.AliasModel = originalModel

	// 构造 ProviderInfo：将实际 model 写入 Name（GeminiClient.BuildURL 据此拼 URL）。
	// 取副本，避免修改路由缓存中的共享 info。
	callInfo := makeTranslationInfo(info, realModel)

	// ── 翻译到目标 Provider 协议 ──
	providerTranslator, downstreamReq := g.translateToProvider(callInfo, internalReq)

	// ── 执行 Provider 调用 ──
	ctx := r.Context()
	if stream {
		g.handleStreamRequest(ctx, w, r, providerClient, callInfo, downstreamReq, providerTranslator, ingressTranslator, ingressProtocol, internalReq, startTime)
	} else {
		g.handleNonStreamResponse(ctx, w, r, providerClient, callInfo, downstreamReq, providerTranslator, ingressTranslator, ingressProtocol, internalReq, startTime)
	}
}

// makeTranslationInfo 为翻译路径构造 ProviderInfo：把路由解析出的实际 model 写入 Name，
// 让 provider（尤其是 GeminiClient）的 BuildURL 能拿到正确的模型名构建 URL。
// 返回副本以避免修改路由缓存中的共享 ProviderInfo。
func makeTranslationInfo(info *schema.ProviderInfo, model string) *schema.ProviderInfo {
	return &schema.ProviderInfo{
		Name:     model,
		BaseURL:  info.BaseURL,
		APIToken: info.APIToken,
		Version:  info.Version,
	}
}

// SetAliasFile 设置模型别名映射
func (g *Gateway) SetAliasFile(af *db.AliasFile) {
	g.aliasFile = af
}

// resolveAlias 解析客户端模型名，返回 (真实模型名, 客户端原始模型名, 是否命中别名映射)
// Gateway 模式下 alias 文件优先于 ModelRouter 的 ModelToProvider 映射
func (g *Gateway) resolveAlias(clientModel string) (real string, original string, hit bool) {
	if g.aliasFile == nil || clientModel == "" {
		return clientModel, clientModel, false
	}

	rawVal, ok := g.aliasFile.Lookup(clientModel)
	if !ok {
		return clientModel, clientModel, false
	}

	switch {
	case rawVal == "@default":
		// @default: 路由到默认 provider 的第一个模型
		for _, pc := range g.cfg.Providers {
			if len(pc.Models) > 0 {
				// 用第一个 provider 的第一个模型
				return pc.Models[0], clientModel, true
			}
		}
		return rawVal, clientModel, true
	case strings.HasPrefix(rawVal, "@db:"):
		rest := strings.TrimPrefix(rawVal, "@db:")
		parts := strings.SplitN(rest, ",", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
			return strings.TrimSpace(parts[1]), clientModel, true
		}
		return rawVal, clientModel, true
	default:
		return rawVal, clientModel, true
	}
}

// 让 provider 的 BuildURL / DefaultHeaders 能拿到正确的 model 与 APIToken。
// 返回副本以避免修改路由缓存中的共享 ProviderInfo。
func makePassthroughInfo(info *schema.ProviderInfo, model string) *schema.ProviderInfo {
	return &schema.ProviderInfo{
		Name:     model,
		BaseURL:  info.BaseURL,
		APIToken: info.APIToken,
	}
}

// handlePassthroughNonStream 透传非流式：请求/响应都不翻译，原样转发
func (g *Gateway) handlePassthroughNonStream(ctx context.Context, w http.ResponseWriter, r *http.Request,
	client provider.Provider, info *schema.ProviderInfo, rawBody json.RawMessage,
	realModel string, aliasModel string, aliasHit bool, ingressProtocol string, startTime time.Time) {

	callInfo := makePassthroughInfo(info, realModel)
	resp, headers, err := client.Call(ctx, rawBody, callInfo)
	if err != nil {
		latency := time.Since(startTime).Milliseconds()
		g.recordRequest(r, startTime, info.Name, http.StatusInternalServerError, latency, err.Error())
		g.sendError(w, ingressProtocol, http.StatusInternalServerError, "provider error", err.Error())
		return
	}

	// 别名回显：将响应 JSON 中的 model 字段替换为客户端原始模型名
	if aliasHit && aliasModel != "" {
		var bodyMap map[string]interface{}
		if json.Unmarshal(resp, &bodyMap) == nil {
			bodyMap["model"] = aliasModel
			resp, _ = json.Marshal(bodyMap)
		}
	}

	for k, v := range headers {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(resp)

	latency := time.Since(startTime).Milliseconds()
	g.recordRequest(r, startTime, info.Name, http.StatusOK, latency, "")
}

// handlePassthroughStream 透传流式：下游 SSE 过滤元数据后原样转发
func (g *Gateway) handlePassthroughStream(ctx context.Context, w http.ResponseWriter, r *http.Request,
	client provider.Provider, info *schema.ProviderInfo, rawBody json.RawMessage,
	realModel string, ingressProtocol string, startTime time.Time) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		g.sendError(w, ingressProtocol, http.StatusInternalServerError, "streaming not supported", "server does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	callInfo := makePassthroughInfo(info, realModel)
	lines, headers, err := client.CallStream(ctx, rawBody, callInfo)
	if err != nil {
		flusher.Flush()
		g.sendError(w, ingressProtocol, http.StatusInternalServerError, "stream error", err.Error())
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

	g.recordRequest(r, startTime, "", http.StatusOK, time.Since(startTime).Milliseconds(), "")
}

// extractModelFromRaw 从原始 JSON 中提取 model 字段，不解构整个请求体
func extractModelFromRaw(body json.RawMessage, ingressProtocol string) string {
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

// detectStream 检测请求体中 stream 字段
func detectStream(body json.RawMessage) bool {
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

// handleChatCompletion 入口：Chat Completions 协议
func (g *Gateway) handleChatCompletion(w http.ResponseWriter, r *http.Request) {
	g.handleRequest(w, r, "chatcompletion")
}

// handleMessages 入口：Anthropic Messages 协议
func (g *Gateway) handleMessages(w http.ResponseWriter, r *http.Request) {
	g.handleRequest(w, r, "anthropic")
}

// handleResponses 入口：OpenAI Responses 协议
func (g *Gateway) handleResponses(w http.ResponseWriter, r *http.Request) {
	g.handleRequest(w, r, "responses")
}

// handleGenerateContent 入口：Google Gemini GenerateContent 协议
func (g *Gateway) handleGenerateContent(w http.ResponseWriter, r *http.Request) {
	g.handleRequest(w, r, "gemini")
}

// handleModelsCatchAll 通配 /v1/models/*，用于处理带冒号分隔的 Gemini 路径
// 请求格式：/v1/models/{model}:generateContent
// chi 不识别冒号作为路径分隔符，因此用 * 通配 + 应用层判断
func (g *Gateway) handleModelsCatchAll(w http.ResponseWriter, r *http.Request) {
	suffix := chi.URLParam(r, "*")
	if !strings.HasSuffix(suffix, ":generateContent") {
		http.NotFound(w, r)
		return
	}
	// 从通配路径中提取模型名：/{model}:generateContent → {model}
	model := strings.TrimSuffix(suffix, ":generateContent")
	model = strings.TrimPrefix(model, "/")
	// 将模型名写入 ctx，供 GeminiTranslator.TranslateRequest 兜底使用
	ctx := gemini.WithGeminiModel(r.Context(), model)
	g.handleGenerateContent(w, r.WithContext(ctx))
}

// handleNonStreamResponse 非流式响应
func (g *Gateway) handleNonStreamResponse(ctx context.Context, w http.ResponseWriter, r *http.Request,
	client provider.Provider, info *schema.ProviderInfo, downstreamReq json.RawMessage,
	providerTranslator interface{}, ingressTranslator translator.CombinedTranslator,
	ingressProtocol string, internalReq *schema.InternalRequest, startTime time.Time) {

	resp, headers, err := client.Call(ctx, downstreamReq, info)
	if err != nil {
		latency := time.Since(startTime).Milliseconds()
		g.recordRequest(r, startTime, info.Name, http.StatusInternalServerError, latency, err.Error())
		g.sendError(w, ingressProtocol, http.StatusInternalServerError, "provider error", err.Error())
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
			latency := time.Since(startTime).Milliseconds()
			g.recordRequest(r, startTime, info.Name, http.StatusInternalServerError, latency, err.Error())
			g.sendError(w, ingressProtocol, http.StatusInternalServerError, "translate response failed", err.Error())
			return
		}
	} else {
		// OpenAI 兼容：直接解析为 InternalResponse（用 CC 翻译器转换）
		var ccResp chatcompletion.ChatCompletionResponse
		if err := json.Unmarshal(resp, &ccResp); err != nil {
			latency := time.Since(startTime).Milliseconds()
			g.recordRequest(r, startTime, info.Name, http.StatusInternalServerError, latency, err.Error())
			g.sendError(w, ingressProtocol, http.StatusInternalServerError, "parse response failed", err.Error())
			return
		}
		internalResp = chatCompletionToInternal(&ccResp)
	}

	// ── 出站翻译：InternalResponse → 入站协议格式 ──
	if internalReq != nil && internalReq.AliasModel != "" {
		internalResp.Model = internalReq.AliasModel
	}
	outgoingResp, err := ingressTranslator.TranslateResponse(internalResp)
	if err != nil {
		latency := time.Since(startTime).Milliseconds()
		g.recordRequest(r, startTime, info.Name, http.StatusInternalServerError, latency, err.Error())
		g.sendError(w, ingressProtocol, http.StatusInternalServerError, "encode response failed", err.Error())
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
	g.recordRequest(r, startTime, info.Name, http.StatusOK, latency, "")
}

// handleStreamRequest 流式请求
func (g *Gateway) handleStreamRequest(ctx context.Context, w http.ResponseWriter, r *http.Request,
	client provider.Provider, info *schema.ProviderInfo, downstreamReq json.RawMessage,
	providerTranslator interface{}, ingressTranslator translator.CombinedTranslator,
	ingressProtocol string, internalReq *schema.InternalRequest, startTime time.Time) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		g.sendError(w, ingressProtocol, http.StatusInternalServerError, "streaming not supported", "server does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	lines, _, err := client.CallStream(ctx, downstreamReq, info)
	if err != nil {
		flusher.Flush()
		g.sendError(w, ingressProtocol, http.StatusInternalServerError, "stream error", err.Error())
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
				// 处理响应头
				continue
			}

			// Provider 翻译器解析流式事件
			if providerTranslator != nil {
				pte := providerTranslator.(interface {
					TranslateStreamEvent(json.RawMessage) *schema.InternalStreamEvent
				})
				event := pte.TranslateStreamEvent(line)
				if event != nil {
					if internalReq != nil && internalReq.AliasModel != "" && event.Data != nil {
						event.Data.Model = internalReq.AliasModel
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
				if choice.Delta.Content != "" {
					msg.Content, _ = json.Marshal(choice.Delta.Content)
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
				modelName := ccChunk.Model
				if internalReq != nil && internalReq.AliasModel != "" {
					modelName = internalReq.AliasModel
				}
				events <- schema.InternalStreamEvent{
					Type: eventType,
					Data: &schema.InternalStreamChunk{
						ID:    ccChunk.ID,
						Model: modelName,
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
	g.recordRequest(r, startTime, info.Name, http.StatusOK, latency, "")
}

// translateToProvider 根据目标 Provider 类型选择翻译器并构建下游请求体
func (g *Gateway) translateToProvider(info *schema.ProviderInfo, internalReq *schema.InternalRequest) (interface{}, json.RawMessage) {
	providerType := info.Version

	switch providerType {
	case "openai", "sensenova":
		// OpenAI 兼容：直接透传（使用 InternalRequest 的 Model）
		ccReq := buildCCRequest(internalReq)
		downstreamReq, _ := json.Marshal(ccReq)
		return nil, downstreamReq

	case "anthropic":
		pt := anthropic.NewAnthropicTranslator(info.APIVersion)
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
		// 兜底：直接透传
		downstreamReq, _ := json.Marshal(buildCCRequest(internalReq))
		return nil, downstreamReq
	}
}

// buildCCRequest 将 InternalRequest 构建为 OpenAI 兼容请求体
func buildCCRequest(req *schema.InternalRequest) *chatcompletion.ChatCompletionRequest {
	messages := make([]chatcompletion.Message, 0, len(req.Messages)+1)

	// System prompt
	if len(req.SystemPrompt) > 0 {
		var text string
		json.Unmarshal(req.SystemPrompt, &text)
		if text != "" {
			messages = append(messages, chatcompletion.Message{
				Role:    "system",
				Content: makeContent(text),
			})
		}
	}

	// Messages
	for _, msg := range req.Messages {
		if msg.Role == schema.RoleSystem {
			continue
		}
		im := chatcompletion.Message{
			Role: string(msg.Role),
			Name: msg.Name,
		}
		// 优先使用 ContentBlocks（含图片等多模态内容），否则回退到 Content 纯文本
		if len(msg.ContentBlocks) > 0 {
			im.Content = buildCCContentFromBlocks(msg.ContentBlocks)
		} else if msg.Content != nil {
			var text string
			json.Unmarshal(msg.Content, &text)
			im.Content = makeContent(text)
		}
		for _, tc := range msg.ToolCalls {
			im.ToolCalls = append(im.ToolCalls, chatcompletion.ToolCall{
				ID:   tc.ID,
				Type: tc.Type,
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
		messages = append(messages, im)
	}

	tools := make([]chatcompletion.Tool, len(req.Tools))
	for i, tool := range req.Tools {
		if tool.Function != nil {
			tools[i] = chatcompletion.Tool{
				Type: "function",
				Function: &chatcompletion.FunctionDef{
					Name:        tool.Function.Name,
					Description: tool.Function.Description,
					Parameters:  tool.Function.Parameters,
				},
			}
		}
	}

	// Stop 序列化
	var stop json.RawMessage
	if len(req.StopSequences) > 0 {
		if len(req.StopSequences) == 1 {
			stop, _ = json.Marshal(req.StopSequences[0])
		} else {
			stop, _ = json.Marshal(req.StopSequences)
		}
	}

	// ResponseFormat
	var respFmt *chatcompletion.ResponseFormat
	if req.ResponseFormat != nil {
		respFmt = &chatcompletion.ResponseFormat{Type: req.ResponseFormat.Type}
	}

	return &chatcompletion.ChatCompletionRequest{
		Model:          req.Model,
		Messages:       messages,
		Tools:          tools,
		Stream:         req.Stream,
		Temperature:    req.Temperature,
		TopP:           req.TopP,
		MaxTokens:      req.MaxTokens,
		Stop:           stop,
		ResponseFormat: respFmt,
		User:           req.UserID,
		Seed:           req.Seed,
	}
}

// makeContent 通过 json.Marshal + Unmarshal 构造 chatcompletion.Content（绕过 struct 构造）
func makeContent(text string) chatcompletion.Content {
	var c chatcompletion.Content
	raw, _ := json.Marshal(text)
	json.Unmarshal(raw, &c)
	return c
}

// buildCCContentFromBlocks 将 InternalContentBlock 数组转换为 CC 格式的内容块数组
// CC 格式: [{type:"text", text:"..."},{type:"image_url", image_url:{url:"data:image/..."}}]
// 内部 Data 字段可能含 data URL 前缀或纯 base64，统一处理
func buildCCContentFromBlocks(blocks []schema.InternalContentBlock) chatcompletion.Content {
	var ccBlocks []json.RawMessage
	for _, b := range blocks {
		switch b.Type {
		case "text":
			block, _ := json.Marshal(map[string]any{"type": "text", "text": b.Text})
			ccBlocks = append(ccBlocks, block)
		case "image":
			url := b.Data
			if url != "" && !strings.HasPrefix(url, "data:") {
				mt := b.MediaType
				if mt == "" {
					mt = "image/png"
				}
				url = "data:" + mt + ";base64," + url
			}
			block, _ := json.Marshal(map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": url},
			})
			ccBlocks = append(ccBlocks, block)
		}
	}
	if len(ccBlocks) == 0 {
		return chatcompletion.Content{}
	}
	raw, _ := json.Marshal(ccBlocks)
	var c chatcompletion.Content
	json.Unmarshal(raw, &c)
	return c
}
func chatCompletionToInternal(ccResp *chatcompletion.ChatCompletionResponse) *schema.InternalResponse {
	var choices []schema.InternalChoice
	for _, c := range ccResp.Choices {
		msg := schema.InternalMessage{
			Role: schema.Role(c.Message.Role),
		}
		// content 可能是字符串或 content block 数组（含图片）
		raw := c.Message.Content.Raw()
		if len(raw) > 0 {
			var text string
			if err := json.Unmarshal(raw, &text); err == nil {
				msg.Content, _ = json.Marshal(text)
			} else {
				var blocks []json.RawMessage
				if err := json.Unmarshal(raw, &blocks); err == nil {
					msg.ContentBlocks = chatcompletion.ParseCCContentBlocks(blocks)
				}
			}
		}
		for _, tc := range c.Message.ToolCalls {
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
		choices = append(choices, schema.InternalChoice{
			Index:        c.Index,
			Message:      msg,
			FinishReason: c.FinishReason,
		})
	}

	var usage *schema.InternalUsage
	if ccResp.Usage != nil {
		usage = &schema.InternalUsage{
			PromptTokens:     ccResp.Usage.PromptTokens,
			CompletionTokens: ccResp.Usage.CompletionTokens,
			TotalTokens:      ccResp.Usage.TotalTokens,
		}
	}

	return &schema.InternalResponse{
		ID:      ccResp.ID,
		Object:  ccResp.Object,
		Created: ccResp.Created,
		Model:   ccResp.Model,
		Choices: choices,
		Usage:   usage,
	}
}

// sendError 发送错误响应（按入站协议格式）
func (g *Gateway) sendError(w http.ResponseWriter, protocol string, code int, errType string, message string) {
	errResp := map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    errType,
			"code":    fmt.Sprintf("%d", code),
		},
	}
	data, _ := json.Marshal(errResp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(data)
}

// mapInternalUsage 将 CC Usage 映射为 InternalUsage
func mapInternalUsage(u *chatcompletion.Usage) *schema.InternalUsage {
	if u == nil {
		return nil
	}
	return &schema.InternalUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
}

// recordRequest 记录请求到监控
func (g *Gateway) recordRequest(r *http.Request, startTime time.Time, provider string, statusCode int, latencyMs int64, errorMsg string) {
	g.store.Record(schema.RequestRecord{
		Time:       startTime,
		Method:     r.Method,
		Path:       r.URL.Path,
		Model:      "",
		Provider:   provider,
		StatusCode: statusCode,
		LatencyMs:  latencyMs,
		ErrorMsg:   errorMsg,
	})
}

func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	models := make([]map[string]interface{}, 0)
	for name, pc := range g.cfg.Providers {
		for _, model := range pc.Models {
			models = append(models, map[string]interface{}{
				"id":     model,
				"object": "model",
				"owner":  name,
			})
		}
	}

	resp := map[string]interface{}{
		"object": "list",
		"data":   models,
	}

	// 若有别名映射，追加别名模型到列表 + metadata.aliases
	if g.aliasFile != nil && len(g.aliasFile.Entries()) > 0 {
		aliases := g.aliasFile.Entries()
		mapping := make(map[string]string, len(aliases))
		existing := make(map[string]bool)
		for _, m := range models {
			existing[m["id"].(string)] = true
		}
		for alias, target := range aliases {
			mapping[alias] = target
			if !existing[alias] {
				models = append(models, map[string]interface{}{
					"id":      alias,
					"object":  "model",
					"owner":   "proxy-alias",
					"aliased": true,
				})
			}
		}
		resp["data"] = models
		resp["metadata"] = map[string]interface{}{
			"aliases": mapping,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func (g *Gateway) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := map[string]interface{}{
		"uptime":         time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"active_conns":   g.store.GetSummary()["active_conns"],
		"total_requests": g.store.GetSummary()["total_requests"],
		"providers":      g.router.GetProviderNames(),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

package server

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
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
	verboseLevel       int // 0=关闭 1=-v 2=-vv
}

func NewGateway(cfg *config.Config, verboseLevel int) *Gateway {
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
		verboseLevel:       verboseLevel,
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
// @AI_GUARD: GATEWAY_HANDLE_REQUEST_ENTRY - 复杂模式总入口，所有路由决策的起点
// @CONSTRAINT: 必须与 quick.go handleRequest 保持同步
//   - 透传路径：入站协议==上游协议，直接转发，仅替换 model 名
//   - 翻译路径：入站协议≠上游协议，经过 Central Schema 完整翻译
//   - 模型别名解析必须在路由之前
//   - stream 检测影响后续 handler 选择
//
// @RELATED: quick.go handleRequest (快速模式入口，必须保持同步)
func (g *Gateway) handleRequest(w http.ResponseWriter, r *http.Request, ingressProtocol string) {
	startTime := time.Now()
	if g.verboseLevel >= 2 {
		defer func() {
			log.Printf("[request] total → %v (path=%s, protocol=%s)", time.Since(startTime), r.URL.Path, ingressProtocol)
		}()
	}
	g.store.IncrActiveConns()
	defer g.store.DecrActiveConns()

	// 读取请求体
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		if g.verboseLevel >= 2 {
			log.Printf("[error] read body failed: %v", err)
		}
		g.sendError(w, ingressProtocol, http.StatusBadRequest, "invalid JSON", err.Error())
		return
	}

	// 速率限制
	if g.cfg.RateLimit.Enabled {
		if !g.rateLimiter.Allow() {
			if g.verboseLevel >= 2 {
				log.Printf("[error] rate limited: remote=%s", r.RemoteAddr)
			}
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
		if g.verboseLevel >= 2 {
			log.Printf("[error] missing model: ingress=%s", ingressProtocol)
		}
		g.sendError(w, ingressProtocol, http.StatusBadRequest, "missing model", "model is required")
		return
	}

	// ── 模型别名解析（优先于 ModelRouter） ──
	realModel, originalModel, aliasHit := g.resolveAlias(model)
	if g.verboseLevel >= 2 {
		if aliasHit {
			log.Printf("[route] alias resolved: %s → %s (hit)", originalModel, realModel)
		} else {
			log.Printf("[route] alias resolved: %s (no mapping, passthrough)", realModel)
		}
	}

	// ── 路由 Provider ──
	info, providerName, err := g.router.Resolve(realModel)
	if err != nil {
		if g.verboseLevel >= 2 {
			log.Printf("[error] model not found: %s → %v", realModel, err)
		}
		g.sendError(w, ingressProtocol, http.StatusNotFound, "model not found", err.Error())
		return
	}

	providerClient := g.registry.Get(providerName)
	if providerClient == nil {
		if g.verboseLevel >= 2 {
			log.Printf("[error] provider not found: name=%s, model=%s", providerName, realModel)
		}
		g.sendError(w, ingressProtocol, http.StatusInternalServerError, "provider not found", providerName)
		return
	}

	// 轻量检测 stream 字段
	stream := detectStream(body)

	// ── 透传 vs 翻译 ──
	// ingress 协议 == provider 协议时，直接透传原始 body，零损耗
	providerType := info.Version
	if g.verboseLevel >= 2 {
		log.Printf("[route] protocol ingress=%s → provider=%s", ingressProtocol, providerType)
	}

	if ingressProtocol == providerType {
		if g.verboseLevel >= 2 {
			log.Printf("[route] PASSTHROUGH: model=%s, ingress=%s, provider=%s", realModel, ingressProtocol, providerType)
		}
		ctx := r.Context()
		if stream {
			// @AI_GUARD: LARGE_BODY_SKIP_STREAM - 大请求跳过原生流式（与 quick.go 同步）
			// @REASON: SenseNova 等上游对大请求（>100KB）流式处理会失败，客户端等不及断开。
			// gateway.go 无 NonStreamAsSSE 包装函数，大请求仍走 stream 路径（依赖 v0.2.73 fallback）
			if g.verboseLevel >= 2 {
				if len(body) > 100*1024 {
					log.Printf("[route] passthrough stream=true, large body (%d bytes) — gateway.go fallback to non-stream may be slow", len(body))
				} else {
					log.Printf("[route] passthrough stream=true")
				}
			}
			g.handlePassthroughStream(ctx, w, r, providerClient, info, body, realModel, ingressProtocol, startTime)
		} else {
			if g.verboseLevel >= 2 {
				log.Printf("[route] passthrough stream=false")
			}
			g.handlePassthroughNonStream(ctx, w, r, providerClient, info, body, realModel, originalModel, aliasHit, ingressProtocol, startTime)
		}
		return
	}

	// ── 入站翻译：协议请求 → InternalRequest ──
	if g.verboseLevel >= 2 {
		log.Printf("[route] TRANSLATION: model=%s, ingress=%s → provider=%s", realModel, ingressProtocol, providerType)
	}
	ingressTranslator := g.translatorRegistry.Get(ingressProtocol)
	if ingressTranslator == nil {
		if g.verboseLevel >= 2 {
			log.Printf("[error] unknown protocol: %s", ingressProtocol)
		}
		g.sendError(w, ingressProtocol, http.StatusInternalServerError, "unknown protocol", ingressProtocol)
		return
	}

	internalReq, err := ingressTranslator.TranslateRequest(r.Context(), body)
	if err != nil {
		if g.verboseLevel >= 2 {
			log.Printf("[error] translate request failed: ingress=%s, model=%s, err=%v", ingressProtocol, realModel, err)
		}
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
		if g.verboseLevel >= 2 {
			log.Printf("[route] translation stream=true, calling handleStreamRequest")
		}
		g.handleStreamRequest(ctx, w, r, providerClient, callInfo, downstreamReq, providerTranslator, ingressTranslator, ingressProtocol, internalReq, startTime)
	} else {
		if g.verboseLevel >= 2 {
			log.Printf("[route] translation stream=false, calling handleNonStreamResponse")
		}
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

	if g.verboseLevel >= 2 {
		handlerStart := time.Now()
		log.Printf("[handler] gateway-passthrough-nonstream: model=%s, alias=%s", realModel, aliasModel)
		defer func() {
			log.Printf("[handler] gateway-passthrough-nonstream → %v (alias=%s, model=%s)", time.Since(handlerStart), aliasModel, realModel)
		}()
	}

	callInfo := makePassthroughInfo(info, realModel)
	upstreamStart := time.Now()
	resp, headers, err := client.Call(ctx, rawBody, callInfo)
	if g.verboseLevel >= 2 {
		log.Printf("[upstream] Call %s → %v", realModel, time.Since(upstreamStart))
	}
	if err != nil {
		if g.verboseLevel >= 2 {
			log.Printf("[error] gateway-passthrough-nonstream → Call failed: model=%s, err=%v", realModel, err)
		}
		latency := time.Since(startTime).Milliseconds()
		g.recordRequest(r, startTime, info.Name, http.StatusInternalServerError, latency, err.Error())
		g.sendError(w, ingressProtocol, http.StatusInternalServerError, "provider error", err.Error())
		return
	}

	// 过滤非标准 thinking 内容块（SenseNova DeepSeek 特有，缺少 signature 字段）
	resp = stripThinkingContentBlocks(resp)

	// 过滤 tool_use.input 中 Claude Code 不接受的多余 description 字段
	// @REASON: Fable 5 在 Anthropic tool_use 的 input 中额外添加 description 字段，
	//         Claude Code v2.1.233+ 的 Bash tool schema 不接受该字段，导致 "tool call could not be parsed"
	resp = stripToolUseDescription(resp)

	// 修复 usage 为 null 的问题：Claude Code 解析 K.usage.input_tokens 时若为 null 会报 undefined
	resp = fixNullUsageInResponse(resp)

	// 别名回显：将响应 JSON 中的 model 字段替换为客户端原始模型名
	if aliasHit && aliasModel != "" {
		resp = echoAliasInResponseBody(resp, aliasModel)
		if g.verboseLevel >= 2 {
			log.Printf("[passthrough] alias echo: %s → %s in response body", realModel, aliasModel)
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

	if g.verboseLevel >= 2 {
		log.Printf("[handler] gateway-passthrough-nonstream → response=%d bytes", len(resp))
	}

	latency := time.Since(startTime).Milliseconds()
	g.recordRequest(r, startTime, info.Name, http.StatusOK, latency, "")
}

// @AI_GUARD: GATEWAY_PASSTHROUGH_STREAM - 必须与 quick.go handlePassthroughStreamWithBody 保持同步
// @CONSTRAINT: 修改此函数时必须同步修改 quick.go 对应的 handlePassthroughStreamWithBody
//   - WriteHeader(200)+Flush 必须在 CallStream 之前（chunked transfer 防超时）
//   - callDone/callFinished 心跳同步不可移除（防止并发写 ResponseWriter）
//   - SSE 换行必须为 \n\n（双换行），不可改为 \n
//   - 所有 SSE 数据行必须带 data: 前缀
//   - 心跳格式必须为 event: ping\ndata: {"type":"ping"}\n\n（Anthropic 标准 ping 事件，必须包含 event: 前缀）
//
// @RELATED: quick.go handlePassthroughStreamWithBody, heartbeatEvent
// @REASON: 历史血泪教训 - 与 quick.go 不同步导致复杂模式无法正常工作
// handlePassthroughStream 透传流式：下游 SSE 过滤元数据后原样转发
func (g *Gateway) handlePassthroughStream(ctx context.Context, w http.ResponseWriter, r *http.Request,
	client provider.Provider, info *schema.ProviderInfo, rawBody json.RawMessage,
	realModel string, ingressProtocol string, startTime time.Time) {

	if g.verboseLevel >= 2 {
		handlerStart := time.Now()
		log.Printf("[handler] gateway-passthrough-stream: model=%s", realModel)
		defer func() {
			log.Printf("[handler] gateway-passthrough-stream → %v (model=%s)", time.Since(handlerStart), realModel)
		}()
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		g.sendError(w, ingressProtocol, http.StatusInternalServerError, "streaming not supported", "server does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // 立即发送响应头，防止客户端在等待上游首个响应时超时断开

	// ⚠️ Responses 协议完全禁用 SSE 心跳 / ping：Codex 严格校验 event 类型，
	//   ping 事件会破坏 Responses API 事件序列完整性。
	isPTResponses := ingressProtocol == "responses"

	// 在 CallStream 阻塞等待上游首个响应期间发送 SSE 心跳（Responses 除外）
	var callDone chan struct{}
	var callFinished chan struct{}
	if isPTResponses {
		callDone, callFinished = newDummyHeartbeat()
	} else {
		callDone, callFinished = StartSSEHeartbeat(w, flusher, r.Context(), g.verboseLevel)
	}

	callInfo := makePassthroughInfo(info, realModel)

	// 上游类型自适应字段过滤：必须在发送上游前处理
	body := applyUpstreamStrip(info.BaseURL, rawBody)

	upstreamStart := time.Now()
	lines, headers, err := client.CallStream(ctx, body, callInfo)
	if g.verboseLevel >= 2 {
		log.Printf("[upstream] CallStream %s → %v", realModel, time.Since(upstreamStart))
	}

	close(callDone) // 停止 CallStream 期间的心跳
	<-callFinished  // 等待心跳 goroutine 退出，防止并发写

	if err != nil {
		// @AI_GUARD: STREAM_FALLBACK_NONSTREAM - 流式失败降级为非流式 SSE 包装
		// @CONSTRAINT: SSE 头已发送（WriteHeader(200)），不能改回 JSON，必须用 writeNonStreamAsSSE 包装
		// @REASON: 上游（如 SenseNova）对大请求（>100KB）的流式处理可能立即失败（context canceled），
		//          但非流式 Call 能正常返回。降级确保客户端仍收到完整 SSE 响应。
		log.Printf("[passthrough] upstream stream error: model=%s url=%s body_len=%d err=%v — fallback to non-stream→SSE",
			realModel, info.BaseURL, len(rawBody), err)
		if g.verboseLevel >= 2 {
			log.Printf("[passthrough] stream failed, fallback to non-stream→SSE")
		}
		// @AI_GUARD: FALLBACK_DETACHED_CONTEXT - fallback 必须用独立 Background ctx，不能用请求 ctx
		nsBody := quickRemoveStreamFlag(rawBody)
		nsBody = applyUpstreamStrip(info.BaseURL, nsBody)
		hbCtx := ctx
		if hbCtx.Err() != nil {
			hbCtx = context.Background()
		}
		var fbCallDone chan struct{}
		var fbCallFinished chan struct{}
		if isPTResponses {
			fbCallDone, fbCallFinished = newDummyHeartbeat()
		} else {
			fbCallDone, fbCallFinished = StartSSEHeartbeat(w, flusher, hbCtx, g.verboseLevel)
		}
		callDone2, callFinished2 := fbCallDone, fbCallFinished
		fbCtx, fbCancel := context.WithTimeout(context.Background(), time.Duration(info.TimeoutSec)*time.Second)
		respBody, _, err2 := client.Call(fbCtx, nsBody, callInfo)
		fbCancel()
		close(callDone2)
		<-callFinished2
		if err2 != nil {
			log.Printf("[passthrough] fallback non-stream also failed: model=%s ctx_err=%v err=%v", realModel, ctx.Err(), err2)
			sendSSEErrorFromUpstream(w, flusher, fmt.Errorf("stream error: %w; fallback non-stream error: %w", err, err2))
			return
		}
		// 过滤 thinking + 修复 usage（同 quick.go handlePassthroughNonStreamAsSSE）
		respBody = stripThinkingContentBlocks(respBody)
		respBody = stripToolUseDescription(respBody)
		respBody = fixNullUsageInResponse(respBody)
		writeNonStreamAsSSE(w, flusher, respBody, realModel)
		return
	}

	// 透传下游响应头
	for k, v := range headers {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}

	// 流式 thinking 过滤状态
	var inThinkingBlock bool
	streamHeartbeatStart := time.Now()
	streamHeartbeatCount := 0
	var streamHeartbeat *time.Ticker
	// Responses 协议绝不发送 streamHeartbeat：Codex SSE 解析器状态机严格只接受 Responses 官方事件集
	if !isPTResponses {
		streamHeartbeat = time.NewTicker(500 * time.Millisecond)
		w.Write(heartbeatEvent)
		flusher.Flush()
	} else {
		// Dummy ticker：永远不会触发（避免下面 case <-streamHeartbeat.C: nil channel 永久阻塞）
		streamHeartbeat = &time.Ticker{C: make(chan time.Time)}
	}
	defer func() {
		if streamHeartbeat != nil {
			streamHeartbeat.Stop()
		}
		if g.verboseLevel >= 2 {
			log.Printf("[heartbeat] stream stopped → %v (total=%d beats)", time.Since(streamHeartbeatStart), streamHeartbeatCount)
		}
	}()

	for {
		select {
		case line, ok := <-lines:
			if !ok {
				goto streamDone
			}
			streamHeartbeat.Reset(500 * time.Millisecond)
			var meta map[string]interface{}
			if json.Unmarshal(line, &meta) == nil {
				if meta["_type"] == "headers" {
					continue
				}
				// @AI_GUARD: STREAM_FALLBACK_NONSTREAM - 流式 channel 内部错误降级
				// @REASON: 上游（SenseNova）CallStream 成功但 channel 首事件为 _type=error+_status=502，
				//          必须降级为非流式 Call + SSE 包装，不能直接透传错误 JSON
				if meta["_type"] == "error" {
					status, _ := meta["_status"].(float64)
					if status == 0 {
						status = 502
					}
					errData, _ := meta["data"].(string)
					log.Printf("[passthrough] upstream stream error (in-channel): model=%s url=%s body_len=%d status=%v err=%s — fallback to non-stream→SSE",
						realModel, info.BaseURL, len(rawBody), status, errData)
					if g.verboseLevel >= 2 {
						log.Printf("[passthrough] in-channel stream failed, fallback to non-stream→SSE")
					}
					streamHeartbeat.Stop()
					// @AI_GUARD: FALLBACK_DETACHED_CONTEXT - 与请求 ctx 解绑，避免链式取消
					nsBody := quickRemoveStreamFlag(rawBody)
					nsBody = applyUpstreamStrip(info.BaseURL, nsBody)
					hbCtx := ctx
					if hbCtx.Err() != nil {
						hbCtx = context.Background()
					}
					var fb2CallDone chan struct{}
					var fb2CallFinished chan struct{}
					if isPTResponses {
						fb2CallDone, fb2CallFinished = newDummyHeartbeat()
					} else {
						fb2CallDone, fb2CallFinished = StartSSEHeartbeat(w, flusher, hbCtx, g.verboseLevel)
					}
					callDone2, callFinished2 := fb2CallDone, fb2CallFinished
					fbCtx, fbCancel := context.WithTimeout(context.Background(), time.Duration(info.TimeoutSec)*time.Second)
					respBody, _, err2 := client.Call(fbCtx, nsBody, callInfo)
					fbCancel()
					close(callDone2)
					<-callFinished2
					if err2 != nil {
						log.Printf("[passthrough] fallback non-stream also failed: model=%s ctx_err=%v err=%v", realModel, ctx.Err(), err2)
						// 先发 message_start 再发 error
						msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
						w.Write([]byte(`event: message_start` + "\n" +
							fmt.Sprintf(`data: {"type":"message_start","message":{"id":"%s","type":"message","role":"assistant","content":[],"model":"%s","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`, msgID, realModel) + "\n\n"))
						sendSSEErrorFromUpstream(w, flusher, fmt.Errorf("stream error: %s; fallback non-stream error: %w", errData, err2))
						return
					}
					respBody = stripThinkingContentBlocks(respBody)
					respBody = fixNullUsageInResponse(respBody)
					writeNonStreamAsSSE(w, flusher, respBody, realModel)
					return
				}
			}

			// 确保 SSE 数据行有 data: 前缀
			lineStr := string(line)
			if !strings.HasPrefix(lineStr, "data: ") && !strings.HasPrefix(lineStr, ":") && !strings.HasPrefix(lineStr, "event:") {
				lineStr = "data: " + lineStr
			}

			// 过滤非标准 thinking 内容块（SenseNova DeepSeek 特有，缺少 signature 字段）
			if strings.HasPrefix(lineStr, "data: ") {
				payload := lineStr[6:] // 去掉 "data: " 前缀
				var evt map[string]interface{}
				if json.Unmarshal([]byte(payload), &evt) == nil {
					evtType, _ := evt["type"].(string)
					switch evtType {
					case "content_block_start":
						var ct string
						if cb, ok := evt["content_block"].(map[string]interface{}); ok {
							if cbtype, ok := cb["type"].(string); ok {
								ct = cbtype
							}
							if ct == "thinking" {
								if _, hasSig := cb["signature"]; !hasSig {
									inThinkingBlock = true
									continue
								}
							}
							if ct == "tool_use" {
								if input, ok := cb["input"].(map[string]interface{}); ok {
									delete(input, "description")
									filteredEvt, _ := json.Marshal(evt)
									lineStr = "data: " + string(filteredEvt)
								}
							}
						}
					case "content_block_delta":
						if inThinkingBlock {
							continue
						}
					case "content_block_stop":
						if inThinkingBlock {
							inThinkingBlock = false
							continue
						}
					}
				}
			}

			w.Write([]byte(lineStr))
			w.Write([]byte("\n\n"))
			flusher.Flush()
		case <-streamHeartbeat.C:
			streamHeartbeatCount++
			w.Write(heartbeatEvent)
			flusher.Flush()
			if g.verboseLevel >= 2 {
				log.Printf("[heartbeat] stream sent #%d → %v", streamHeartbeatCount, time.Since(streamHeartbeatStart))
			}
		}
	}
streamDone:

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
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		g.handleResponsesWebSocket(w, r)
		return
	}
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

	if g.verboseLevel >= 2 {
		handlerStart := time.Now()
		log.Printf("[handler] gateway-translation-nonstream: model=%s, stream=%v", internalReq.Model, internalReq.Stream)
		defer func() {
			log.Printf("[handler] gateway-translation-nonstream → %v (model=%s)", time.Since(handlerStart), internalReq.Model)
		}()
	}

	upstreamStart := time.Now()
	resp, headers, err := client.Call(ctx, downstreamReq, info)
	if g.verboseLevel >= 2 {
		log.Printf("[upstream] Call %s → %v", internalReq.Model, time.Since(upstreamStart))
	}
	if err != nil {
		if g.verboseLevel >= 2 {
			log.Printf("[error] gateway-translation-nonstream → Call failed: model=%s, err=%v", internalReq.Model, err)
		}
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
			if g.verboseLevel >= 2 {
				log.Printf("[error] gateway-translation-nonstream → TranslateFromProvider failed: model=%s, err=%v", internalReq.Model, err)
			}
			latency := time.Since(startTime).Milliseconds()
			g.recordRequest(r, startTime, info.Name, http.StatusInternalServerError, latency, err.Error())
			g.sendError(w, ingressProtocol, http.StatusInternalServerError, "translate response failed", err.Error())
			return
		}
	} else {
		// OpenAI 兼容：直接解析为 InternalResponse（用 CC 翻译器转换）
		var ccResp chatcompletion.ChatCompletionResponse
		if err := json.Unmarshal(resp, &ccResp); err != nil {
			if g.verboseLevel >= 2 {
				log.Printf("[error] gateway-translation-nonstream → json.Unmarshal failed: model=%s, err=%v", internalReq.Model, err)
			}
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
		if g.verboseLevel >= 2 {
			log.Printf("[error] gateway-translation-nonstream → TranslateResponse failed: model=%s, err=%v", internalReq.Model, err)
		}
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

	if g.verboseLevel >= 2 {
		log.Printf("[handler] gateway-translation-nonstream → response=%d bytes", len(outgoingResp))
	}

	latency := time.Since(startTime).Milliseconds()
	g.recordRequest(r, startTime, info.Name, http.StatusOK, latency, "")
}

// @AI_GUARD: GATEWAY_STREAM_REQUEST - 必须与 quick.go handleStreamRequest 保持同步
// @CONSTRAINT: 修改此函数时必须同步修改 quick.go 对应的 handleStreamRequest
//   - WriteHeader(200)+Flush 必须在 CallStream 之前
//   - callDone/callFinished 心跳同步不可移除
//   - 上游 SSE 行必须剥离 "data: " 前缀后再传入 TranslateStreamEvent
//   - TranslateStreamEvent 类型断言签名必须为 json.RawMessage
//
// @RELATED: quick.go handleStreamRequest, anthropic/translator.go TranslateStream
// @REASON: 历史血泪教训 - 与 quick.go 不同步导致翻译路径在复杂模式下失败
// handleStreamRequest 流式请求
func (g *Gateway) handleStreamRequest(ctx context.Context, w http.ResponseWriter, r *http.Request,
	client provider.Provider, info *schema.ProviderInfo, downstreamReq json.RawMessage,
	providerTranslator interface{}, ingressTranslator translator.CombinedTranslator,
	ingressProtocol string, internalReq *schema.InternalRequest, startTime time.Time) {

	if g.verboseLevel >= 2 {
		handlerStart := time.Now()
		log.Printf("[handler] gateway-translation-stream: model=%s, stream=%v", internalReq.Model, internalReq.Stream)
		defer func() {
			log.Printf("[handler] gateway-translation-stream → %v (model=%s)", time.Since(handlerStart), internalReq.Model)
		}()
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		g.sendError(w, ingressProtocol, http.StatusInternalServerError, "streaming not supported", "server does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // 立即发送响应头，防止客户端在等待上游首个响应时超时断开

	// ⚠️ Responses 协议完全禁用 SSE 心跳：Codex 客户端严格校验 event 类型，
	//   event: ping 不属于 OpenAI Responses API 事件集，插入后会导致其解析器
	//   状态机异常，报 "stream closed before response.completed"。
	//   Responses API 通过流动的 SSE 数据事件保持连接，无需额外心跳。
	isResponsesIngress := ingressProtocol == "responses"

	// ── 阶段 1: CallStream 期间的心跳 ──
	var callDone1 chan struct{}
	var callFinished1 chan struct{}
	if isResponsesIngress {
		callDone1, callFinished1 = newDummyHeartbeat()
	} else {
		callDone1, callFinished1 = StartSSEHeartbeat(w, flusher, r.Context(), g.verboseLevel)
	}

	upstreamStart := time.Now()
	lines, _, err := client.CallStream(ctx, downstreamReq, info)
	if g.verboseLevel >= 2 {
		log.Printf("[upstream] CallStream %s → %v", internalReq.Model, time.Since(upstreamStart))
	}

	close(callDone1) // 停止阶段 1 心跳
	<-callFinished1  // 等待心跳 goroutine 退出

	if err != nil {
		// SSE 错误事件，避免 superfluous response.WriteHeader
		if g.verboseLevel >= 2 {
			log.Printf("[sse-error] gateway-stream → CallStream failed: %v", err)
		}
		// 先发 message_start（Claude Code 解析器要求 SSE 流以 message_start 开头）
		// id 不能为空，否则 Claude Code 报 "empty or malformed response"
		msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
		w.Write([]byte(`event: message_start` + "\n" +
			fmt.Sprintf(`data: {"type":"message_start","message":{"id":"%s","type":"message","role":"assistant","content":[],"model":"","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`, msgID) + "\n\n"))
		errData, _ := json.Marshal(map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"type":    "stream_error",
				"message": err.Error(),
			},
		})
		w.Write([]byte("event: error\ndata: "))
		w.Write(errData)
		w.Write([]byte("\n\n"))
		// 发送 message_stop 终止 SSE 流（带 event: 前缀）
		w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		flusher.Flush()
		return
	}

	// ── 阶段 2: 流处理期间的心跳（保护等待上游数据到达的空窗期） ──
	// 使用 MutexSSEWriter 防止心跳 goroutine 与 TranslateStream 回调并发写 w
	mw := NewMutexSSEWriter(w, flusher)
	var callDone2 chan struct{}
	var callFinished2 chan struct{}
	if isResponsesIngress {
		callDone2, callFinished2 = newDummyHeartbeat()
	} else {
		callDone2, callFinished2 = StartSSEHeartbeat(mw, mw, r.Context(), g.verboseLevel)
	}

	// 构建内部流式事件 channel
	events := make(chan schema.InternalStreamEvent, 16)
	go func() {
		defer close(events)
		ccStartSent := false // OpenAI 兼容路径是否已发送 start 事件
		for line := range lines {
			// 跳过元数据
			var meta map[string]interface{}
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
					// 首个事件就是错误时，先发 start 让 Responses 翻译器生成 response.created
					if !ccStartSent {
						ccStartSent = true
						modelName := ""
						if internalReq != nil {
							modelName = internalReq.Model
						}
						events <- schema.InternalStreamEvent{
							Type: "start",
							Data: &schema.InternalStreamChunk{
								Model: modelName,
								Choices: []schema.InternalChoice{{
									Message: schema.InternalMessage{Role: schema.RoleAssistant},
								}},
							},
						}
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

			// 剥离 data: 前缀（与 quick.go 保持一致）
			payload := strings.TrimPrefix(string(line), "data: ")
			if payload == "[DONE]" {
				continue
			}

			// Provider 翻译器解析流式事件
			if providerTranslator != nil {
				pte := providerTranslator.(interface {
					TranslateStreamEvent(json.RawMessage) *schema.InternalStreamEvent
				})
				event := pte.TranslateStreamEvent(json.RawMessage(payload))
				if event != nil {
					if internalReq != nil && internalReq.AliasModel != "" && event.Data != nil {
						event.Data.Model = internalReq.AliasModel
					}
					events <- *event
				}
			} else {
				// OpenAI 兼容：解析 SSE delta 行
				var ccChunk chatcompletion.ChatCompletionStreamChunk
				if json.Unmarshal([]byte(payload), &ccChunk) != nil || len(ccChunk.Choices) == 0 {
					continue
				}
				// 首个有效 delta 事件前先发 start，Responses 入站翻译器据此生成 response.created
				if !ccStartSent {
					ccStartSent = true
					modelName := ccChunk.Model
					if internalReq != nil && internalReq.AliasModel != "" {
						modelName = internalReq.AliasModel
					}
					events <- schema.InternalStreamEvent{
						Type: "start",
						Data: &schema.InternalStreamChunk{
							ID:    ccChunk.ID,
							Model: modelName,
							Choices: []schema.InternalChoice{{
								Index:   ccChunk.Choices[0].Index,
								Message: schema.InternalMessage{Role: schema.RoleAssistant},
							}},
						},
					}
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
				evt := schema.InternalStreamEvent{
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
				events <- evt
			}
		}
	}()

	// 使用入站翻译器的 TranslateStream 写出站 SSE
	// 阶段 2 心跳保护整个流处理过程（等待上游数据到达 + TranslateStream 写入）
	ingressTranslator.TranslateStream(ctx, events, func(eventData []byte, isDone bool) {
		mw.Write(eventData)
		mw.Flush()
	})

	// 流处理完成，停止阶段 2 心跳
	close(callDone2)
	<-callFinished2

	latency := time.Since(startTime).Milliseconds()
	g.recordRequest(r, startTime, info.Name, http.StatusOK, latency, "")
}

// @AI_GUARD: GATEWAY_TRANSLATE_TO_PROVIDER - InternalRequest → 上游协议请求（Central Schema 出口）
// @CONSTRAINT: 必须与 quick.go translateToProvider 保持同步
//   - 根据 providerType 选择正确的翻译器并构建下游请求体
//   - 返回 (providerTranslator, downstreamReq) 供后续 handler 使用
//   - 新增协议时必须在此 switch 中注册对应的翻译器
//
// @RELATED: quick.go translateToProvider, all protocol/translator.go TranslateToProvider 方法
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

// @AI_GUARD: BUILD_CC_REQUEST - InternalRequest → CC 请求体构建，消息格式转换的核心函数
// @CONSTRAINT: 修改消息映射逻辑必须同步检查所有调用方
//   - SystemPrompt → messages[0] role:system
//   - ContentBlocks 优先于 Content（多模态还原）
//   - ToolCalls.Arguments 是 JSON 字符串（不可 Marshal 为对象）
//   - Stop 序列化：string/[]string 两种格式
//
// @RELATED: quick.go buildCCRequest (两个文件各自有独立实现，修改需同步)
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
		StreamOptions:  &chatcompletion.StreamOptions{IncludeUsage: true},
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
					"id":       alias,
					"object":   "model",
					"created":  time.Now().Unix(),
					"owned_by": "proxy-alias",
					"owner":    "proxy-alias",
					"aliased":  true,
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

// ═══════════════════════════════════════════════════════════════════════════════
//  WebSocket 支持 — OpenAI Codex 通过 WebSocket 连接 /v1/responses
// ═══════════════════════════════════════════════════════════════════════════════

const (
	wsGUID         = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsOpText  byte = 0x1 // text frame
	wsOpClose byte = 0x8 // close frame
	wsFin          = 0x80
	wsMask         = 0x80
)

// computeAcceptKey 计算 WebSocket Sec-WebSocket-Accept 值
func computeAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// readWSFrame 从连接读取一个 WebSocket 帧，返回 payload 数据
func readWSFrame(conn net.Conn) ([]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}

	masked := header[1]&wsMask != 0
	length := uint64(header[1] & 0x7F)

	switch {
	case length == 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(conn, ext); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext))
	case length == 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(conn, ext); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(ext)
	}

	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(conn, maskKey[:]); err != nil {
			return nil, err
		}
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, err
	}

	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}

	return payload, nil
}

// writeWSFrame 向连接写入一个 WebSocket 文本帧
func writeWSFrame(conn net.Conn, payload []byte) error {
	length := len(payload)

	var frame []byte
	switch {
	case length < 126:
		frame = make([]byte, 2+length)
		frame[0] = wsFin | wsOpText
		frame[1] = byte(length)
		copy(frame[2:], payload)
	case length < 65536:
		frame = make([]byte, 4+length)
		frame[0] = wsFin | wsOpText
		frame[1] = 126
		binary.BigEndian.PutUint16(frame[2:4], uint16(length))
		copy(frame[4:], payload)
	default:
		frame = make([]byte, 10+length)
		frame[0] = wsFin | wsOpText
		frame[1] = 127
		binary.BigEndian.PutUint64(frame[2:10], uint64(length))
		copy(frame[10:], payload)
	}

	_, err := conn.Write(frame)
	return err
}

// wsResponseWriter 实现 http.ResponseWriter + http.Flusher，将 HTTP 响应写入 WebSocket 帧
type wsResponseWriter struct {
	conn        net.Conn
	header      http.Header
	wroteHeader bool
}

func (w *wsResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *wsResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if len(b) == 0 {
		return 0, nil
	}
	if err := writeWSFrame(w.conn, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (w *wsResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
}

// Flush implements http.Flusher. WebSocket 帧已是原子单位，无需额外缓冲。
func (w *wsResponseWriter) Flush() {}

// handleResponsesWebSocket 处理 OpenAI Codex 的 WebSocket 升级请求
// 将 WebSocket 协议转换为内部 HTTP 处理，再通过 WebSocket 帧返回响应
func (g *Gateway) handleResponsesWebSocket(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket not supported", http.StatusInternalServerError)
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	// WebSocket 握手
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return
	}
	acceptKey := computeAcceptKey(key)

	bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	bufrw.WriteString("Upgrade: websocket\r\n")
	bufrw.WriteString("Connection: Upgrade\r\n")
	bufrw.WriteString("Sec-WebSocket-Accept: " + acceptKey + "\r\n")
	bufrw.WriteString("\r\n")
	if err := bufrw.Flush(); err != nil {
		return
	}

	// 读取第一个 WebSocket 帧（JSON 请求体）
	payload, err := readWSFrame(conn)
	if err != nil {
		return
	}

	// 构造内部 HTTP 请求，复用 handleRequest 处理逻辑
	wsReq, err := http.NewRequestWithContext(r.Context(), "POST", r.URL.String(), io.NopCloser(strings.NewReader(string(payload))))
	if err != nil {
		return
	}
	wsReq.Header = r.Header.Clone()
	wsReq.Header.Del("Upgrade")
	wsReq.Header.Del("Connection")
	wsReq.Header.Del("Sec-WebSocket-Key")
	wsReq.Header.Del("Sec-WebSocket-Version")

	wsWriter := &wsResponseWriter{conn: conn}
	g.handleRequest(wsWriter, wsReq, "responses")
}

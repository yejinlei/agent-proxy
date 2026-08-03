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
	mux.Post("/v1/models/{model}/generateContent", g.handleGenerateContent)

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

	// ── 入站翻译：协议请求 → InternalRequest ──
	ingressTranslator := g.translatorRegistry.Get(ingressProtocol)
	if ingressTranslator == nil {
		g.sendError(w, ingressProtocol, http.StatusInternalServerError, "unknown protocol", ingressProtocol)
		return
	}

	internalReq, err := ingressTranslator.TranslateRequest(body)
	if err != nil {
		g.sendError(w, ingressProtocol, http.StatusBadRequest, "translate request failed", err.Error())
		return
	}

	// ── 路由 Provider ──
	info, providerName, err := g.router.Resolve(internalReq.Model)
	if err != nil {
		g.sendError(w, ingressProtocol, http.StatusNotFound, "model not found", err.Error())
		return
	}

	// ── 翻译到目标 Provider 协议 ──
	providerTranslator, downstreamReq := g.translateToProvider(info, internalReq)

	// ── 执行 Provider 调用 ──
	providerClient := g.registry.Get(providerName)
	if providerClient == nil {
		g.sendError(w, ingressProtocol, http.StatusInternalServerError, "provider not found", providerName)
		return
	}

	stream := internalReq.Stream
	ctx := r.Context()
	if stream {
		g.handleStreamRequest(ctx, w, r, providerClient, info, downstreamReq, providerTranslator, ingressTranslator, ingressProtocol, startTime)
	} else {
		g.handleNonStreamResponse(ctx, w, r, providerClient, info, downstreamReq, providerTranslator, ingressTranslator, ingressProtocol, internalReq, startTime)
	}
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
	ingressProtocol string, startTime time.Time) {

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
					events <- *event
				}
			} else {
				// OpenAI 兼容：透传，构建 delta 事件
				events <- schema.InternalStreamEvent{
					Type: "delta",
					Data: &schema.InternalStreamChunk{
						Choices: []schema.InternalChoice{{
							Index: 0,
							Message: schema.InternalMessage{
								Role:    schema.RoleAssistant,
								Content: line,
							},
						}},
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
		if msg.Content != nil {
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
func chatCompletionToInternal(ccResp *chatcompletion.ChatCompletionResponse) *schema.InternalResponse {
	var choices []schema.InternalChoice
	for _, c := range ccResp.Choices {
		msg := schema.InternalMessage{
			Role: schema.Role(c.Message.Role),
		}
		if c.Message.Content != "" {
			msg.Content = json.RawMessage(`"` + c.Message.Content + `"`)
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
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func (g *Gateway) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := map[string]interface{}{
		"uptime":         time.Since(time.Now()).String(),
		"active_conns":   g.store.GetSummary()["active_conns"],
		"total_requests": g.store.GetSummary()["total_requests"],
		"providers":      g.router.GetProviderNames(),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

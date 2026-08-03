package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

	// 初始化翻译器
	translatorRegistry := translator.NewTranslatorRegistry()
	translatorRegistry.Register(&chatcompletion.ChatCompletionTranslator{})

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

	// 初始化路由器
	modelRouter := router.NewModelRouter()
	modelRouter.SetDefaultProvider(cfg.ModelRouter.DefaultProvider)
	modelRouter.SetPrefixMatch(cfg.ModelRouter.PrefixMatch)
	for model, providerName := range cfg.ModelRouter.ModelToProvider {
		modelRouter.AddRoute(model, providerName)
	}
	for name, pc := range cfg.Providers {
		modelRouter.AddProvider(name, pc.ToProviderInfo())
	}

	// Web UI
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

func (g *Gateway) Routes() chi.Router {
	mux := chi.NewRouter()

	// 中间件
	mux.Use(middleware.Logger())
	mux.Use(middleware.Recoverer)
	mux.Use(middleware.CORS)

	// 认证
	if g.cfg.Auth.APIKey != "" {
		mux.Use(middleware.Auth(g.cfg.Auth.APIKey))
	}

	// API 路由
	mux.Post("/v1/chat/completions", g.handleChatCompletion)
	mux.Post("/v1/responses", g.handleResponses)
	mux.Post("/v1/messages", g.handleMessages)
	mux.Post("/v1/models", g.handleModels)
	mux.Post("/v1/models/{model}/generateContent", g.handleGenerateContent)

	// Health check
	mux.Get("/health", g.handleHealth)
	mux.Get("/status", g.handleStatus)

	// Web UI
	if g.cfg.Monitor.Enabled {
		mux.Mount(g.cfg.Monitor.UIPath, g.webServer.Handle())
	}

	return mux
}

// handleChatCompletion 处理 OpenAI Chat Completions 请求
func (g *Gateway) handleChatCompletion(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	g.store.IncrActiveConns()
	defer g.store.DecrActiveConns()

	ctx := r.Context()
	defer ctx.Done()

	// 读取请求体
	var ccReq chatcompletion.ChatCompletionRequest
	body, err := readJSONBody(r, &ccReq)
	if err != nil {
		g.sendError(w, r, http.StatusBadRequest, "invalid JSON", err.Error())
		return
	}

	// 速率限制
	if g.cfg.RateLimit.Enabled {
		if !g.rateLimiter.Allow() {
			g.sendError(w, r, http.StatusTooManyRequests, "rate limited", "exceeded rate limit")
			return
		}
	}

	// 路由到 Provider
	info, providerName, err := g.router.Resolve(ccReq.Model)
	if err != nil {
		g.sendError(w, r, http.StatusNotFound, "model not found", err.Error())
		return
	}

	// 翻译请求
	translator := g.translatorRegistry.Get("chatcompletion")
	internalReq, err := translator.TranslateRequest(body)
	if err != nil {
		g.sendError(w, r, http.StatusInternalServerError, "translate failed", err.Error())
		return
	}

	// 确定目标 Provider 类型
	providerClient := g.registry.Get(providerName)
	if providerClient == nil {
		g.sendError(w, r, http.StatusInternalServerError, "provider not found", providerName)
		return
	}

	// 根据 Provider 类型选择翻译器并构造下游请求体
	var providerTranslator interface {
		TranslateFromProvider(json.RawMessage) (*schema.InternalResponse, error)
	}

	providerType := info.Version
	var downstreamReq json.RawMessage

	switch providerType {
	case "openai", "sensenova":
		downstreamReq = body
	case "anthropic":
		providerTranslator = anthropic.NewAnthropicTranslator(info.Version)
		req, _ := providerTranslator.(*anthropic.AnthropicTranslator).TranslateToProvider(internalReq)
		downstreamReq, _ = json.Marshal(req)
	case "gemini":
		providerTranslator = gemini.NewGeminiTranslator()
		req, _ := providerTranslator.(*gemini.GeminiTranslator).TranslateToProvider(internalReq)
		downstreamReq, _ = json.Marshal(req)
	case "responses":
		providerTranslator = responses.NewResponsesTranslator()
		req, _ := providerTranslator.(*responses.ResponsesTranslator).TranslateToProvider(internalReq)
		downstreamReq, _ = json.Marshal(req)
	default:
		downstreamReq = body
	}

	// 流式请求
	if ccReq.Stream {
		g.handleStreamRequest(ctx, w, r, providerClient, info, downstreamReq, providerTranslator, internalReq, startTime)
		return
	}

	// 非流式请求
	resp, headers, err := providerClient.Call(ctx, downstreamReq, info)
	if err != nil {
		latency := time.Since(startTime).Milliseconds()
		g.recordRequest(r, startTime, providerName, http.StatusInternalServerError, latency, err.Error())
		g.sendError(w, r, http.StatusInternalServerError, "provider error", err.Error())
		return
	}

	// 翻译响应
	var ccResp *chatcompletion.ChatCompletionResponse
	if providerTranslator != nil {
		internalResp, err := providerTranslator.TranslateFromProvider(resp)
		if err != nil {
			latency := time.Since(startTime).Milliseconds()
			g.recordRequest(r, startTime, providerName, http.StatusInternalServerError, latency, err.Error())
			g.sendError(w, r, http.StatusInternalServerError, "translate response failed", err.Error())
			return
		}
		// 转换为 CC 响应
		ccResp = internalToCCResponse(internalResp)
	} else {
		// 直接解析为 CC 响应（OpenAI 兼容）
		ccResp = &chatcompletion.ChatCompletionResponse{}
		if err := json.Unmarshal(resp, ccResp); err != nil {
			latency := time.Since(startTime).Milliseconds()
			g.recordRequest(r, startTime, providerName, http.StatusInternalServerError, latency, err.Error())
			g.sendError(w, r, http.StatusInternalServerError, "parse response failed", err.Error())
			return
		}
	}

	// 透传下游响应头
	for k, v := range headers {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}

	// 写入响应
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(ccResp)

	// 记录
	latency := time.Since(startTime).Milliseconds()
	g.recordRequest(r, startTime, providerName, http.StatusOK, latency, "")
}

// handleStreamRequest 处理流式请求
func (g *Gateway) handleStreamRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, client provider.Provider, info *schema.ProviderInfo, downstreamReq json.RawMessage, providerTranslator interface{}, internalReq *schema.InternalRequest, startTime time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		g.sendError(w, r, http.StatusInternalServerError, "streaming not supported", "server does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// 获取流式事件
	lines, _, err := client.CallStream(ctx, downstreamReq, info)
	if err != nil {
		flusher.Flush()
		g.sendError(w, r, http.StatusInternalServerError, "stream error", err.Error())
		return
	}

	// 流式翻译（providerTranslator 非 nil 时直接透传原始 SSE 行）
	if providerTranslator != nil {
		for line := range lines {
			// 跳过元数据事件
			var meta map[string]interface{}
			if json.Unmarshal(line, &meta) == nil && meta["_type"] == "headers" {
				for k, v := range meta["_headers"].(map[string]interface{}) {
					if headers, ok := v.([]interface{}); ok {
						for _, h := range headers {
							w.Header().Add(k, fmt.Sprintf("%v", h))
						}
					}
				}
				continue
			}

			// 直接透传下游 SSE 行（后续可在 providerTranslator 上做全量流式翻译）
			fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		}
	} else {
		// 无翻译器：直接透传下游 SSE
		for line := range lines {
			fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		}
	}

	// 发送 [DONE]
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	// 记录
	latency := time.Since(startTime).Milliseconds()
	g.recordRequest(r, startTime, info.Name, http.StatusOK, latency, "")
}

// readJSONBody 读取并解析 JSON 请求体
func readJSONBody(r *http.Request, v interface{}) (json.RawMessage, error) {
	maxBodySize := int64(1 << 20) // 1MB
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return nil, err
	}
	return body, nil
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

// sendError 发送错误响应
func (g *Gateway) sendError(w http.ResponseWriter, r *http.Request, code int, errType string, message string) {
	errResp := chatcompletion.ErrorResponse{
		Error: &chatcompletion.CCError{
			Message: message,
			Type:    errType,
			Code:    fmt.Sprintf("%d", code),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(errResp)
}

// handleResponses 处理 Responses API 请求
func (g *Gateway) handleResponses(w http.ResponseWriter, r *http.Request) {
	// 与 handleChatCompletion 逻辑类似，但使用 Responses 翻译器
	g.handleChatCompletion(w, r) // 简化：暂用同一逻辑
}

// handleMessages 处理 Anthropic Messages API 请求
func (g *Gateway) handleMessages(w http.ResponseWriter, r *http.Request) {
	g.handleChatCompletion(w, r)
}

// handleGenerateContent 处理 Gemini GenerateContent 请求
func (g *Gateway) handleGenerateContent(w http.ResponseWriter, r *http.Request) {
	g.handleChatCompletion(w, r)
}

// handleModels 列出可用模型
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

// handleHealth 健康检查
func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

// handleStatus 状态检查
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

// internalToCCResponse 将 InternalResponse 转为 CC 响应
func internalToCCResponse(resp *schema.InternalResponse) *chatcompletion.ChatCompletionResponse {
	ccResp := &chatcompletion.ChatCompletionResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: resp.Created,
		Model:   resp.Model,
		Choices: make([]chatcompletion.Choice, len(resp.Choices)),
	}

	for i, choice := range resp.Choices {
		ccResp.Choices[i] = chatcompletion.Choice{
			Index: choice.Index,
			Message: chatcompletion.CCMessage{
				Role:      string(choice.Message.Role),
				Content:   "",
				ToolCalls: make([]chatcompletion.ToolCall, len(choice.Message.ToolCalls)),
			},
			FinishReason: choice.FinishReason,
		}

		// 提取内容
		if choice.Message.Content != nil {
			var text string
			json.Unmarshal(choice.Message.Content, &text)
			ccResp.Choices[i].Message.Content = text
		}

		// Tool calls
		for j, tc := range choice.Message.ToolCalls {
			ccResp.Choices[i].Message.ToolCalls[j] = chatcompletion.ToolCall{
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
	}

	if resp.Usage != nil {
		ccResp.Usage = &chatcompletion.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		}
	}

	return ccResp
}

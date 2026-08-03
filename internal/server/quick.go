package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/protocol/anthropic"
	"github.com/agent-proxy/agent-proxy/internal/protocol/chatcompletion"
	"github.com/agent-proxy/agent-proxy/internal/protocol/gemini"
	"github.com/agent-proxy/agent-proxy/internal/protocol/responses"
	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
	"github.com/agent-proxy/agent-proxy/internal/provider"
	"github.com/go-chi/chi/v5"
)

// QuickGateway 超简易模式：从 DB 选一条记录，直接转发 + 协议翻译
type QuickGateway struct {
	proxyName string
	info      *schema.ProviderInfo
	provider  provider.Provider
}

// NewQuickGateway 从 DB 记录创建一个超简易网关
func NewQuickGateway(name, baseURL, apiKey, providerType string, timeout int) *QuickGateway {
	var p provider.Provider
	switch providerType {
	case "anthropic":
		p = provider.NewAnthropicClient(name, baseURL, apiKey, "2023-06-01", timeout)
	case "gemini":
		p = provider.NewGeminiClient(name, baseURL, timeout)
	default:
		p = provider.NewOpenAIClient(name, baseURL, timeout)
	}

	return &QuickGateway{
		proxyName: name,
		info: &schema.ProviderInfo{
			Name:     name,
			BaseURL:  baseURL,
			APIToken: apiKey,
			Version:  providerType,
		},
		provider: p,
	}
}

func (q *QuickGateway) Routes() chi.Router {
	mux := chi.NewRouter()

	mux.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"ok","mode":"quick","provider":"%s"}`, q.proxyName)
	})

	mux.Post("/v1/chat/completions", q.handleChatCompletion)
	mux.Post("/v1/messages", q.handleChatCompletion)
	mux.Post("/v1/responses", q.handleChatCompletion)

	return mux
}

func (q *QuickGateway) handleChatCompletion(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		q.sendError(w, http.StatusBadRequest, "read_body", err.Error())
		return
	}

	var ccReq chatcompletion.ChatCompletionRequest
	if err := json.Unmarshal(body, &ccReq); err != nil {
		q.sendError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	// ChatCompletion → Internal
	translator := &chatcompletion.ChatCompletionTranslator{}
	internalReq, err := translator.TranslateRequest(body)
	if err != nil {
		q.sendError(w, http.StatusInternalServerError, "translate", err.Error())
		return
	}

	// 下游请求体构造
	downstreamReq := body
	if q.info.Version != "openai" {
		var downstream interface{}
		switch q.info.Version {
		case "anthropic":
			downstream, _ = anthropic.NewAnthropicTranslator(q.info.Version).TranslateToProvider(internalReq)
		case "gemini":
			downstream, _ = gemini.NewGeminiTranslator().TranslateToProvider(internalReq)
		case "responses":
			downstream, _ = responses.NewResponsesTranslator().TranslateToProvider(internalReq)
		}
		if downstream != nil {
			d, _ := json.Marshal(downstream)
			downstreamReq = d
		}
	}

	if ccReq.Stream {
		q.handleStream(w, downstreamReq, startTime, r)
		return
	}

	// 非流式
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	resp, _, err := q.provider.Call(ctx, downstreamReq, q.info)
	if err != nil {
		q.sendError(w, http.StatusInternalServerError, "provider_error", err.Error())
		return
	}

	ccResp := q.translateResponse(resp)
	if ccResp == nil {
		q.sendError(w, http.StatusInternalServerError, "parse_response", "failed to parse")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ccResp)

	latency := time.Since(startTime).Milliseconds()
	fmt.Printf("[%s] %s → %s %dms\n", time.Now().Format("15:04:05"), ccReq.Model, q.proxyName, latency)
}

func (q *QuickGateway) handleStream(w http.ResponseWriter, req json.RawMessage, startTime time.Time, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		q.sendError(w, http.StatusInternalServerError, "stream", "not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	lines, _, err := q.provider.CallStream(ctx, req, q.info)
	if err != nil {
		q.sendError(w, http.StatusInternalServerError, "stream_error", err.Error())
		return
	}

	for line := range lines {
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
		fmt.Fprintf(w, "data: %s\n\n", line)
		flusher.Flush()
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	latency := time.Since(startTime).Milliseconds()
	fmt.Printf("[%s] stream %s %dms\n", time.Now().Format("15:04:05"), q.proxyName, latency)
}

func (q *QuickGateway) translateResponse(resp json.RawMessage) *chatcompletion.ChatCompletionResponse {
	if q.info.Version == "openai" {
		var cc chatcompletion.ChatCompletionResponse
		if json.Unmarshal(resp, &cc) == nil {
			return &cc
		}
		return nil
	}

	var providerTranslator interface {
		TranslateFromProvider(json.RawMessage) (*schema.InternalResponse, error)
	}

	switch q.info.Version {
	case "anthropic":
		providerTranslator = anthropic.NewAnthropicTranslator(q.info.Version)
	case "gemini":
		providerTranslator = gemini.NewGeminiTranslator()
	case "responses":
		providerTranslator = responses.NewResponsesTranslator()
	}

	if providerTranslator == nil {
		var cc chatcompletion.ChatCompletionResponse
		if json.Unmarshal(resp, &cc) == nil {
			return &cc
		}
		return nil
	}

	internalResp, err := providerTranslator.TranslateFromProvider(resp)
	if err != nil {
		return nil
	}

	return chatcompletion.InternalToCCResponse(internalResp)
}

func (q *QuickGateway) sendError(w http.ResponseWriter, code int, typ, msg string) {
	err := chatcompletion.ErrorResponse{
		Error: &chatcompletion.CCError{
			Message: msg,
			Type:    typ,
			Code:    fmt.Sprintf("%d", code),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(err)
}

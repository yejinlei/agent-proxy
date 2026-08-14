package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/db"
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
	modelsMap          map[string][]string
	modelsMu           sync.RWMutex // 保护 modelsMap/capabilities 的并发读写(ensureModels)
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
	// 模型别名映射文件（外部配置，支持 @default / @db:<id>,<model> / 纯字符串）
	aliasFile *db.AliasFile
	// 流式偏好：按上游地址，首次请求并行竞速 SSE vs 非流式，后续直接用胜出方式
	streamPreferMu sync.RWMutex
	streamPrefer   map[string]bool // baseURL -> true=非流式更快, false=SSE 更快; key 不存在=未探测
}

// 心跳格式：SSE 注释行（RFC 6455 §3.4）
// @AI_GUARD: SSE_HEARTBEAT_FORMAT - 心跳格式绝对不可修改
// @CONSTRAINT: 必须用 SSE 注释格式 ": heartbeat\n\n"，不可改为 data: 行
//   - data: {}\n\n → Claude Code 解析为 Anthropic 事件，缺少 type 字段 → 解析失败
//   - data: \n\n → Kimi 等严格客户端对空行做 JSON.parse 报错
//     -: heartbeat\n\n → RFC 6455 §3.4 注释格式，客户端忽略但重置 TCP 超时计时器
//
// @RELATED: all handlePassthrough* / handleNonStream* handlers that write heartbeat
// @REASON: 历史血泪教训 - 先后尝试过 data: \n\n、data: {}\n\n，均导致不同客户端崩溃
var heartbeatEvent = []byte(": heartbeat\n\n")

// NewQuickGateway 从 DB 记录创建一个超简易网关
// capabilities: 嗅探到的上游协议列表，如 ["openai", "anthropic", "gemini", "responses"]
// modelsMap: 协议→模型列表映射，如 {"openai":["gpt-4"],"anthropic":["claude-3"]}
func NewQuickGateway(name, baseURL, apiKey string, capabilities []string, modelsMap map[string][]string, timeout int, clientKey string, clientKeyEnabled bool, verboseLevel int) *QuickGateway {
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
		modelsMap:          modelsMap,
		translatorRegistry: registry,
		proxyBaseURL:       strings.TrimSuffix(strings.TrimSuffix(baseURL, "/"), "/v1"),
		proxyKey:           apiKey,
		clientKey:          clientKey,
		clientKeyEnabled:   clientKeyEnabled,
		verboseLevel:       verboseLevel,
	}
}

// SetAliasFile 设置模型别名映射（可延迟设置，在 Routes() 之前调用）
func (q *QuickGateway) SetAliasFile(af *db.AliasFile) {
	q.aliasFile = af
}

// resolveAlias 解析客户端模型名：
//   - 若 aliasFile 为 nil，透传原始模型名
//   - 若命中映射，处理 @default / @db:<id>,<model> / 纯字符串三种语法
//   - 返回 (真实模型名, 原始模型名, 是否命中映射)
//
// resolveAlias 解析客户端模型名：
// 支持点号分隔符与连字符之间的归一化（如 claude-haiku-4.5 → claude-haiku-4-5），
// 因为 Claude Code 使用点号作为版本分隔符，而别名文件使用连字符。
func (q *QuickGateway) resolveAlias(clientModel string) (real string, original string, hit bool) {
	if q.aliasFile == nil || clientModel == "" {
		return clientModel, clientModel, false
	}

	// 先尝试原始模型名
	rawVal, ok := q.aliasFile.Resolve(clientModel)
	if !ok {
		// 原始名未命中 → 将点号替换为连字符再试一次
		normalized := strings.ReplaceAll(clientModel, ".", "-")
		if normalized != clientModel {
			rawVal, ok = q.aliasFile.Resolve(normalized)
		}
	}
	if !ok {
		return clientModel, clientModel, false
	}

	switch {
	case rawVal == "@default":
		// @default: 解析为上游 /v1/models 返回的第一个模型（OpenAI 协议）
		// 首次调用时 modelsMap 可能为空 → 同步嗅探上游 /v1/models
		q.modelsMu.RLock()
		models, okM := q.modelsMap["openai"]
		q.modelsMu.RUnlock()
		if !okM || len(models) == 0 {
			q.ensureModels()
			q.modelsMu.RLock()
			models = q.modelsMap["openai"]
			q.modelsMu.RUnlock()
		}
		if len(models) > 0 {
			return models[0], clientModel, true
		}
		// fallback: 使用第一个非空协议
		q.modelsMu.RLock()
		for _, p := range q.capabilities {
			if m, okCap := q.modelsMap[p]; okCap && len(m) > 0 {
				q.modelsMu.RUnlock()
				return m[0], clientModel, true
			}
		}
		q.modelsMu.RUnlock()
		// 二次兜底：仍然失败就透传原始值，上游会报错提示用户
		return clientModel, clientModel, false
	case strings.HasPrefix(rawVal, "@db:"):
		// @db:<id>,<model_name>: 取逗号后面的模型名
		rest := strings.TrimPrefix(rawVal, "@db:")
		parts := strings.SplitN(rest, ",", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
			return strings.TrimSpace(parts[1]), clientModel, true
		}
		return rawVal, clientModel, true
	default:
		// 纯字符串：直接当模型名使用
		return rawVal, clientModel, true
	}
}

// ensureModels 懒加载上游模型列表：首次调用 resolveAlias 遇到 @default 且 modelsMap 为空时触发
// 直接构造 HTTP 请求调用上游 /v1/models，避免共享 provider 状态导致数据竞争
func (q *QuickGateway) ensureModels() {
	// 双重检查：先读锁判断，非空直接返回
	q.modelsMu.RLock()
	if len(q.modelsMap) > 0 {
		q.modelsMu.RUnlock()
		return
	}
	q.modelsMu.RUnlock()

	// 加写锁，再检查一次（并发场景下可能已经被其他 goroutine 填充）
	q.modelsMu.Lock()
	if len(q.modelsMap) > 0 {
		q.modelsMu.Unlock()
		return
	}

	modelsURL := q.proxyBaseURL + "/v1/models"
	req, err := http.NewRequest("GET", modelsURL, nil)
	if err != nil {
		q.modelsMu.Unlock()
		return
	}
	req.Header.Set("Authorization", "Bearer "+q.proxyKey)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: time.Duration(q.timeout) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		q.modelsMu.Unlock()
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		io.Copy(io.Discard, resp.Body)
		q.modelsMu.Unlock()
		return
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	q.modelsMu.Unlock()
	if err != nil {
		return
	}
	var mResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(bodyBytes, &mResp) != nil || len(mResp.Data) == 0 {
		return
	}
	ids := make([]string, 0, len(mResp.Data))
	for _, m := range mResp.Data {
		ids = append(ids, m.ID)
	}
	q.modelsMu.Lock()
	if q.modelsMap == nil {
		q.modelsMap = make(map[string][]string)
	}
	q.modelsMap["openai"] = ids
	q.modelsMu.Unlock()
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
	// 上游 URL
	upstream string

	// -vv 四向日志
	ingressBody  []byte // 入站原始请求体（Guest → 代理，含客户端假模型名）
	upstreamReq  []byte // 最终发送给上游的请求体（代理 → LLM，已替换为真实模型名）
	upstreamResp []byte // 上游原始响应体（LLM → 代理，含真实模型名）
	outgoingBody []byte // 最终发回客户端的响应体（代理 → Guest，已回显客户端模型名）
}

func (q *QuickGateway) Routes() chi.Router {
	mux := chi.NewRouter()

	// 入口兜底：记录每个到达的请求 + panic 恢复，用于排查 ECONNRESET
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Printf("[incoming] %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[panic] %s %s from %s: %v", r.Method, r.URL.Path, r.RemoteAddr, rec)
				}
			}()
			next.ServeHTTP(w, r)
		})
	})

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
// @AI_GUARD: HANDLE_REQUEST_ENTRY - 快速模式总入口，所有路由决策的起点
// @CONSTRAINT: 修改路由逻辑必须理解整个处理流水线（协议识别→路径决策→格式转换→流决策）
//   - 透传路径：入站协议==上游协议，直接转发，仅替换 model 名
//   - 翻译路径：入站协议≠上游协议，经过 Central Schema 完整翻译
//   - 自适应路由：按请求体 stream 字段自动选择处理方式
//
// @RELATED: gateway.go handleRequest (复杂模式入口，必须保持同步)
// 协议感知路由：先按入站协议选择匹配的上游协议（透传优先），无匹配则回退到 openai 翻译转换。
func (q *QuickGateway) handleRequest(w http.ResponseWriter, r *http.Request, ingressProtocol string) {
	startTime := time.Now()
	if q.verboseLevel >= 2 {
		defer func() {
			log.Printf("[request] total → %v (path=%s, protocol=%s)", time.Since(startTime), r.URL.Path, ingressProtocol)
		}()
	}

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
	model := quickExtractModel(body)
	if model == "" && ingressProtocol == "gemini" {
		if m, ok := gemini.GeminiModelFromContext(r.Context()); ok && m != "" {
			model = m
		}
	}

	// ── 模型别名解析 ──
	// resolveAlias 返回 (真实模型名, 客户端原始模型名, 是否命中映射)
	// 命中映射时：上游用 realModel 调用，响应回显 originalModel
	// 未命中映射时：realModel == originalModel，透传
	realModel, originalModel, aliasHit := q.resolveAlias(model)
	if q.verboseLevel >= 2 {
		if aliasHit {
			log.Printf("[route] alias resolved: %s → %s (hit)", originalModel, realModel)
		} else {
			log.Printf("[route] alias resolved: %s (no mapping, passthrough)", realModel)
		}
	}

	// ── 协议感知路由：按模型归属选择上游协议（本地变量，不修改 q.info 共享结构） ──
	providerType := q.selectProtocol(ingressProtocol)
	p := q.getProvider(providerType)
	if q.verboseLevel >= 2 {
		log.Printf("[route] protocol ingress=%s → provider=%s", ingressProtocol, providerType)
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
		model:           realModel,
		upstream:        q.proxyBaseURL,
		ingressBody:     body,
	}

	// @AI_GUARD: STREAM_MODE_ROUTING - 透传路径路由，修改前必须理解完整的处理流水线
	// @CONSTRAINT: 透传路径按 stream 字段自适应路由:
	//   - stream=true: 自适应探测（首次非流式→SSE 包装，后台竞速决定后续策略）
	//   - 无 stream 字段（Claude Code）: 伪装 SSE（非流式上游→SSE 包装）
	//   - stream:false: 透传非流式
	// @RELATED: handleStreamRequest, handleStreamRequestAsNonStream, handleNonStreamResponse,
	//           handleNonStreamResponseAsSSE, handlePassthroughStreamWithBody,
	//           handlePassthroughNonStream, handlePassthroughNonStreamAsSSE
	// @REASON: 历史血泪教训 - 每修正一种模式可能破坏另一种模式的客户端（Claude Code / Kimi / Codex 行为不同）
	if normalizedIngress == providerType && realModel != "" {
		if q.verboseLevel >= 2 {
			log.Printf("[route] PASSTHROUGH: model=%s, ingress=%s, provider=%s", realModel, normalizedIngress, providerType)
		}
		ctx := context.WithValue(r.Context(), verboseCtxKey{}, vctx)
		stream := quickDetectStream(body)

		if stream {
			// 流式偏好：按上游地址，首次直接走非流式（安全默认），响应后后台竞速 SSE
			q.streamPreferMu.RLock()
			preferNonStream, tested := q.streamPrefer[q.proxyBaseURL]
			q.streamPreferMu.RUnlock()
			if !tested {
				// 首次请求：非流式响应，记录耗时，完成后后台探测 SSE 速度
				if q.verboseLevel >= 2 {
					log.Printf("[route] stream=true, auto-probe (first request for %s)", q.proxyBaseURL)
				}
				nsStart := time.Now()
				q.handlePassthroughNonStreamAsSSE(p, ctx, w, r, realModel, originalModel, aliasHit, startTime, body)
				callInfo := q.info
				callInfo.Name = realModel
				go q.probeStreamPrefer(p, callInfo, realModel, time.Since(nsStart))
				return
			} else if preferNonStream {
				if q.verboseLevel >= 2 {
					log.Printf("[route] stream=true, prefer non-stream→SSE")
				}
				q.handlePassthroughNonStreamAsSSE(p, ctx, w, r, realModel, originalModel, aliasHit, startTime, body)
			} else {
				if q.verboseLevel >= 2 {
					log.Printf("[route] stream=true, prefer native stream")
				}
				q.handlePassthroughStream(p, ctx, w, r, realModel, originalModel, aliasHit, startTime)
			}
			// 无 stream 字段（Claude Code）→ 期望 SSE，包装为非流式上游响应转 SSE
			// 显式 stream:false → 期望 raw JSON
		} else if quickStreamExplicitFalse(body) {
			if q.verboseLevel >= 2 {
				log.Printf("[route] stream=false, passthrough non-stream (raw JSON)")
			}
			q.handlePassthroughNonStream(p, ctx, w, r, realModel, originalModel, aliasHit, startTime)
		} else {
			if q.verboseLevel >= 2 {
				log.Printf("[route] no stream field, passthrough non-stream→SSE (Claude Code)")
			}
			q.handlePassthroughNonStreamAsSSE(p, ctx, w, r, realModel, originalModel, aliasHit, startTime, body)
		}
		return
	}

	// ── 入站翻译：协议请求 → InternalRequest ──
	if q.verboseLevel >= 2 {
		log.Printf("[route] TRANSLATION: model=%s, ingress=%s → provider=%s", realModel, normalizedIngress, providerType)
	}
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

	// 覆盖模型名：上游用 realModel，响应回显 originalModel
	internalReq.Model = realModel
	internalReq.AliasModel = originalModel

	// ── 翻译到目标 Provider 协议 ──
	providerTranslator, downstreamReq := q.translateToProvider(providerType, internalReq)

	// ── 执行 Provider 调用 ──
	stream := internalReq.Stream
	ctx := context.WithValue(r.Context(), verboseCtxKey{}, vctx)
	// 把构建后的下游请求体记录到日志上下文（代理→LLM）
	if downstreamReq != nil {
		vctx.upstreamReq = downstreamReq
	}
	ctx = context.WithValue(ctx, verboseCtxKey{}, vctx)
	if stream {
		if q.verboseLevel >= 2 {
			log.Printf("[route] translation stream=true, calling handleStreamRequest")
		}
		q.handleStreamRequest(p, ctx, w, downstreamReq, providerTranslator, ingressTranslator, internalReq, startTime)
	} else {
		if q.verboseLevel >= 2 {
			log.Printf("[route] translation stream=false, calling handleNonStreamResponse")
		}
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
// （各 provider BuildURL 的 model 参数来自 info.Name），并带上 APIToken 用于认证。
func makeQuickPassthroughInfo(info *schema.ProviderInfo, model string) *schema.ProviderInfo {
	return &schema.ProviderInfo{
		Name:     model,
		BaseURL:  info.BaseURL,
		APIToken: info.APIToken,
	}
}

// quickReplaceModelInBody 将请求体 JSON 中的 model 字段从 from 替换为 to
// 使用字符串替换而非 JSON 重新序列化，保持原始 JSON 结构（字段顺序、缩进、数字类型）不变。
func quickReplaceModelInBody(body json.RawMessage, from, to string) json.RawMessage {
	if from == "" || to == "" || from == to {
		return body
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if modelBytes, ok := m["model"]; ok {
		var modelStr string
		if err := json.Unmarshal(modelBytes, &modelStr); err == nil && modelStr == from {
			newModel, _ := json.Marshal(to)
			m["model"] = newModel
			result, err := json.Marshal(m)
			if err != nil {
				return body
			}
			return result
		}
	}
	return body
}

// echoAliasInResponseBody 将响应 JSON 中的 model 字段回显为客户端原始模型名
// 使用字符串替换，保持原始 JSON 结构不变。
func echoAliasInResponseBody(resp json.RawMessage, aliasModel string) json.RawMessage {
	if aliasModel == "" {
		return resp
	}
	// 快速解析当前 model 值
	var cur struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(resp, &cur) != nil || cur.Model == "" {
		return resp
	}
	s := string(resp)
	old := `"model": "` + cur.Model + `"`
	if strings.Contains(s, old) {
		return json.RawMessage(strings.Replace(s, old, `"model": "`+aliasModel+`"`, 1))
	}
	old = `"model":"` + cur.Model + `"`
	if strings.Contains(s, old) {
		return json.RawMessage(strings.Replace(s, old, `"model":"`+aliasModel+`"`, 1))
	}
	return resp
}

// @AI_GUARD: THINKING_BLOCK_FILTER - 只过滤非标准 thinking 块，不可全局过滤
// @CONSTRAINT: 仅过滤缺少 signature 字段的非标准 thinking 块（SenseNova DeepSeek 特有）
//   - 标准 Anthropic thinking 块（含 signature 字段）必须保留，确保 Kimi 等客户端能接收推理过程
//   - 不可无条件过滤所有 "thinking" 类型，否则会破坏标准 Anthropic 客户端的推理功能
//
// @RELATED: handlePassthroughStreamWithBody (流式 thinking 过滤), handlePassthroughNonStream (非流式过滤)
// @REASON: 历史血泪教训 - 初期无条件过滤 thinking 导致 Claude Code 无法显示推理过程
// stripThinkingContentBlocks 从响应 JSON 中移除非标准 thinking 类型内容块。
// SenseNova 的 DeepSeek 模型在 high effort 模式下返回非标准 thinking 类型内容块
// （缺少 Anthropic 标准要求的 signature 字段），Claude Code 等客户端无法解析。
// 标准 Anthropic thinking 块（含 signature 字段）会被保留，确保 Kimi 等客户端能正常接收推理过程。
func stripThinkingContentBlocks(resp json.RawMessage) json.RawMessage {
	if len(resp) == 0 {
		return resp
	}
	// 快速检测：如果 content 数组中没有 thinking 类型，直接返回
	if !bytes.Contains(resp, []byte(`"type":"thinking"`)) &&
		!bytes.Contains(resp, []byte(`"type": "thinking"`)) {
		return resp
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(resp, &raw); err != nil {
		return resp
	}

	contentRaw, ok := raw["content"]
	if !ok {
		return resp
	}
	contentArr, ok := contentRaw.([]interface{})
	if !ok {
		return resp
	}

	// 过滤掉非标准 thinking 块（无 signature 字段），保留标准 thinking 块
	filtered := make([]interface{}, 0, len(contentArr))
	for _, item := range contentArr {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		if t, _ := itemMap["type"].(string); t == "thinking" {
			// 标准 Anthropic thinking 块包含 signature 字段，保留
			if _, hasSig := itemMap["signature"]; hasSig {
				filtered = append(filtered, item)
				continue
			}
			// 非标准 thinking 块（无 signature），跳过
			continue
		}
		filtered = append(filtered, item)
	}

	raw["content"] = filtered
	out, err := json.Marshal(raw)
	if err != nil {
		return resp
	}
	return json.RawMessage(out)
}

// echoAliasInStreamLine 将流式 SSE 行中的 model 字段回显为客户端原始模型名
// 使用字符串替换，保持原始 JSON 结构不变。
func echoAliasInStreamLine(line json.RawMessage, aliasModel string) json.RawMessage {
	if aliasModel == "" {
		return line
	}
	var cur struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(line, &cur) != nil || cur.Model == "" {
		return line
	}
	s := string(line)
	old := `"model": "` + cur.Model + `"`
	if strings.Contains(s, old) {
		return json.RawMessage(strings.Replace(s, old, `"model": "`+aliasModel+`"`, 1))
	}
	old = `"model":"` + cur.Model + `"`
	if strings.Contains(s, old) {
		return json.RawMessage(strings.Replace(s, old, `"model":"`+aliasModel+`"`, 1))
	}
	return line
}

// quickRemoveStreamFlag 将请求体中的 "stream": true 改为 "stream": false
func quickRemoveStreamFlag(body []byte) []byte {
	s := string(body)
	s = strings.Replace(s, `"stream":true`, `"stream":false`, 1)
	s = strings.Replace(s, `"stream": true`, `"stream": false`, 1)
	return []byte(s)
}

// quickStreamExplicitFalse 检测请求体是否显式设置了 stream:false
// Claude Code 的 Anthropic Messages 请求不带 stream 字段，仍期望 SSE。
// 只有显式 false 才表示客户端期望 raw JSON；缺失或 true 都表示期望 SSE。
func quickStreamExplicitFalse(body json.RawMessage) bool {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	v, exists := m["stream"]
	if !exists {
		return false
	}
	b, ok := v.(bool)
	return ok && !b
}

// @AI_GUARD: PASSTHROUGH_STREAM - 透传流式处理，修改前必须同步 gateway.go
// @CONSTRAINT: callDone/callFinished 通道同步模式不可移除，防止并发写 http.ResponseWriter
// @RELATED: gateway.go handlePassthroughStream (必须保持同步), handlePassthroughStreamWithBody (同函数)
// @REASON: 历史血泪教训 - 心跳 goroutine 在 CallStream 返回后可能继续写 w，导致 panic
// handlePassthroughStreamWithBody 与 handlePassthroughStream 一致，但 body 已在调用方读取
func (q *QuickGateway) handlePassthroughStreamWithBody(p provider.Provider, ctx context.Context, w http.ResponseWriter,
	r *http.Request, realModel string, aliasModel string, aliasHit bool, startTime time.Time, body []byte) {

	if q.verboseLevel >= 2 {
		handlerStart := time.Now()
		log.Printf("[handler] passthrough-stream: model=%s, alias=%s", realModel, aliasModel)
		defer func() {
			log.Printf("[handler] passthrough-stream → %v (alias=%s, model=%s)", time.Since(handlerStart), aliasModel, realModel)
		}()
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		q.sendError(w, http.StatusInternalServerError, "streaming not supported", "server does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // 立即发送响应头，防止客户端在等待上游首个响应时超时断开

	// 在 CallStream 阻塞等待上游首个响应期间发送 SSE 心跳
	callDone, callFinished := StartSSEHeartbeat(w, flusher, r.Context(), q.verboseLevel)

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(q.timeout)*time.Second)
	defer cancel()

	callInfo := makeQuickPassthroughInfo(q.info, realModel)
	if aliasHit && aliasModel != "" {
		body = quickReplaceModelInBody(body, aliasModel, realModel)
	}

	upstreamStart := time.Now()
	lines, headers, err := p.CallStream(callCtx, body, callInfo)
	if q.verboseLevel >= 2 {
		log.Printf("[upstream] CallStream %s → %v", realModel, time.Since(upstreamStart))
	}
	close(callDone) // 停止 CallStream 期间的心跳
	<-callFinished  // 等待心跳 goroutine 退出，防止并发写

	if err != nil {
		// @AI_GUARD: STREAM_ERROR_FORMAT - 流式错误必须用标准 Anthropic SSE error 格式
		// @CONSTRAINT: 错误事件格式：event: error\ndata: {"type":"error","error":{"type":"...","message":"..."}}\n\n
		//   - 必须发送 message_stop 终止 SSE 流
		//   - 不能使用 _type/_status 内部字段
		if q.verboseLevel >= 2 {
			log.Printf("[sse-error] passthrough-stream → CallStream failed: %v", err)
		}
		sendSSEErrorFromUpstream(w, flusher, fmt.Errorf("stream error: %w", err))
		return
	}

	for k, v := range headers {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}

	var lastUsage *schema.InternalUsage
	inThinkingBlock := false // 跟踪当前是否在 thinking 内容块中
	streamHeartbeatStart := time.Now()
	streamHeartbeatCount := 0
	heartbeat := time.NewTicker(500 * time.Millisecond)
	defer func() {
		heartbeat.Stop()
		if q.verboseLevel >= 2 {
			log.Printf("[heartbeat] stream stopped → %v (total=%d beats)", time.Since(streamHeartbeatStart), streamHeartbeatCount)
		}
	}()
	w.Write(heartbeatEvent)
	flusher.Flush()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				goto streamDone
			}
			heartbeat.Reset(500 * time.Millisecond)
			var meta map[string]any
			if json.Unmarshal(line, &meta) == nil {
				if meta["_type"] == "headers" {
					// 已在上方透传，跳过
				} else if meta["_type"] == "error" {
					// 上游 provider 内部错误 → 转为标准 Anthropic SSE error 事件
					errData, _ := meta["data"].(string)
					sendSSEErrorBody(w, flusher, "api_error", errData)
					return
				} else {
					// 过滤非标准 thinking 内容块：SenseNova 的 DeepSeek 模型返回
					// 缺少 signature 字段的非标准 thinking 块，Claude Code 无法解析。
					// 标准 Anthropic thinking 块（含 signature）会被保留。
					eventType, _ := meta["type"].(string)
					switch eventType {
					case "content_block_start":
						if cb, ok := meta["content_block"].(map[string]any); ok {
							if ct, _ := cb["type"].(string); ct == "thinking" {
								if _, hasSig := cb["signature"]; hasSig {
									// 标准 thinking 块，保留
								} else {
									inThinkingBlock = true
									continue
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
					usage := q.extractUsage(trimSSEDataPrefix(line))
					if usage != nil {
						lastUsage = usage
					}
					writeLine := line
					if aliasHit && aliasModel != "" {
						writeLine = echoAliasInStreamLine(line, aliasModel)
					}
					// 按 SSE 标准统一添加 data: 前缀
					writeLineStr := string(writeLine)
					if !strings.HasPrefix(writeLineStr, "data: ") &&
						!strings.HasPrefix(writeLineStr, "event: ") &&
						!strings.HasPrefix(writeLineStr, "id: ") &&
						!strings.HasPrefix(writeLineStr, "retry: ") &&
						len(writeLineStr) > 0 && writeLineStr[0] != ':' {
						if writeLineStr[0] == '{' || writeLineStr[0] == '[' {
							writeLine = append([]byte("data: "), writeLine...)
						}
					}
					if !writeSSE(w, writeLine) {
						return
					}
					if !writeSSE(w, []byte("\n\n")) {
						return
					}
					flusher.Flush()
					continue
				}
			} else {
				if !writeSSE(w, line) {
					return
				}
				if !writeSSE(w, []byte("\n\n")) {
					return
				}
				flusher.Flush()
			}
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			streamHeartbeatCount++
			w.Write(heartbeatEvent)
			flusher.Flush()
			if q.verboseLevel >= 2 {
				log.Printf("[heartbeat] stream sent #%d → %v", streamHeartbeatCount, time.Since(streamHeartbeatStart))
			}
		}
	}
streamDone:
	w.Write([]byte("event: done\ndata: {}\n\n"))
	flusher.Flush()

	vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
	vctx.upstreamReq = body
	q.logRequest(vctx, startTime, http.StatusOK, lastUsage, nil)
}

// @AI_GUARD: PASSTHROUGH_NONSTREAM - 透传非流式，必须同步 gateway.go
// @CONSTRAINT: 响应必须经过 stripThinkingContentBlocks 过滤非标准 thinking 块
//   - 别名模型：响应中 model 字段替换为客户端原始模型名
//
// @RELATED: gateway.go handlePassthroughNonStream (必须同步)
// handlePassthroughNonStream 透传非流式：请求/响应都不翻译，原样转发
func (q *QuickGateway) handlePassthroughNonStream(p provider.Provider, ctx context.Context, w http.ResponseWriter,
	r *http.Request, realModel string, aliasModel string, aliasHit bool, startTime time.Time) {

	if q.verboseLevel >= 2 {
		handlerStart := time.Now()
		log.Printf("[handler] passthrough-nonstream: model=%s, alias=%s", realModel, aliasModel)
		defer func() {
			log.Printf("[handler] passthrough-nonstream → %v (alias=%s, model=%s)", time.Since(handlerStart), aliasModel, realModel)
		}()
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(q.timeout)*time.Second)
	defer cancel()

	callInfo := makeQuickPassthroughInfo(q.info, realModel)
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		q.sendError(w, http.StatusBadRequest, "read_body", err.Error())
		return
	}
	// 命中别名映射时，同步改写请求体中的 model 字段（URL 中已用 realModel，body 也要改）
	if aliasHit && aliasModel != "" {
		bodyBefore := string(body)
		body = quickReplaceModelInBody(body, aliasModel, realModel)
		if q.verboseLevel > 0 {
			preview := string(body)
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			log.Printf("[passthrough] alias=%s real=%s body_len=%d→%d preview=%s",
				aliasModel, realModel, len(bodyBefore), len(body), preview)
		}
	}

	upstreamStart := time.Now()
	resp, headers, err := p.Call(callCtx, body, callInfo)
	if q.verboseLevel >= 2 {
		log.Printf("[upstream] Call %s → %v", realModel, time.Since(upstreamStart))
	}
	if err != nil {
		log.Printf("[passthrough] upstream error: %s=%s url=%s body_len=%d err=%v",
			aliasModel, realModel, q.proxyBaseURL, len(body), err)
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

	// 若命中别名映射，将响应中 model 字段回显为原始模型名
	var outResp json.RawMessage
	if aliasHit && aliasModel != "" {
		outResp = echoAliasInResponseBody(resp, aliasModel)
	} else {
		outResp = resp
	}

	// 过滤 thinking 内容块：SenseNova 的 DeepSeek 模型在 high effort 模式下
	// 返回非标准 thinking 类型内容块，Claude Code 客户端无法解析，导致
	// "API returned an empty or malformed response (HTTP 200)" 错误
	outResp = stripThinkingContentBlocks(outResp)

	if _, err := w.Write(outResp); err != nil {
		log.Printf("[passthrough] write error: %s=%s url=%s err=%v",
			aliasModel, realModel, q.proxyBaseURL, err)
		return
	}

	vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
	vctx.upstreamReq = body
	vctx.upstreamResp = resp
	vctx.outgoingBody = outResp
	q.logRequest(vctx, startTime, http.StatusOK, q.extractUsage(outResp), nil)
}

// handlePassthroughStream 透传流式：下游 SSE 过滤元数据后原样转发
func (q *QuickGateway) handlePassthroughStream(p provider.Provider, ctx context.Context, w http.ResponseWriter,
	r *http.Request, realModel string, aliasModel string, aliasHit bool, startTime time.Time) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		q.sendError(w, http.StatusInternalServerError, "streaming not supported", "server does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // 立即发送响应头，防止客户端在等待上游首个响应时超时断开

	// 在 CallStream 阻塞等待上游首个响应期间发送 SSE 心跳
	callDone, callFinished := StartSSEHeartbeat(w, flusher, r.Context(), q.verboseLevel)

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(q.timeout)*time.Second)
	defer cancel()

	callInfo := makeQuickPassthroughInfo(q.info, realModel)
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		// @AI_GUARD: READ_BODY_ERROR - 必须先停止心跳再写错误，防止并发写
		close(callDone) // 停止心跳
		<-callFinished  // 等待心跳 goroutine 退出
		if q.verboseLevel >= 2 {
			log.Printf("[sse-error] passthrough-stream → read body failed: %v", err)
		}
		sendSSEErrorBody(w, flusher, "invalid_request_error", fmt.Sprintf("read body: %v", err))
		return
	}
	// 命中别名映射时，同步改写请求体中的 model 字段
	if aliasHit && aliasModel != "" {
		body = quickReplaceModelInBody(body, aliasModel, realModel)
	}

	upstreamStart := time.Now()
	lines, headers, err := p.CallStream(callCtx, body, callInfo)
	if q.verboseLevel >= 2 {
		log.Printf("[upstream] CallStream %s → %v", realModel, time.Since(upstreamStart))
	}
	close(callDone) // 停止 CallStream 期间的心跳
	<-callFinished  // 等待心跳 goroutine 退出，防止并发写

	if err != nil {
		// SSE 头已设置，不能调用 sendError，直接写 SSE 错误事件
		log.Printf("[passthrough] upstream stream error: %s=%s url=%s body_len=%d err=%v",
			aliasModel, realModel, q.proxyBaseURL, len(body), err)
		sendSSEErrorFromUpstream(w, flusher, fmt.Errorf("stream error: %w", err))
		return
	}

	// 透传下游响应头
	for k, v := range headers {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}

	var lastUsage *schema.InternalUsage
	streamHeartbeatStart := time.Now()
	streamHeartbeatCount := 0
	heartbeat := time.NewTicker(500 * time.Millisecond)
	defer func() {
		heartbeat.Stop()
		if q.verboseLevel >= 2 {
			log.Printf("[heartbeat] stream stopped → %v (total=%d beats)", time.Since(streamHeartbeatStart), streamHeartbeatCount)
		}
	}()
	// 立即发送首个 SSE 事件，防止客户端在等待上游首个响应时超时断开
	// 心跳格式 data: \n\n（空 data 行），Claude Code 将其识别为内容活动来重置超时
	w.Write(heartbeatEvent)
	flusher.Flush()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				goto streamDone
			}
			heartbeat.Reset(500 * time.Millisecond)
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
					errData, _ := meta["data"].(string)
					log.Printf("[passthrough] upstream stream error: %s=%s url=%s body_len=%d status=%v err=%s",
						aliasModel, realModel, q.proxyBaseURL, len(body), status, errData)
					// SSE 流已开始后不能再修改 HTTP 状态码，写入标准 SSE 错误事件
					errJSON, _ := json.Marshal(map[string]interface{}{
						"type": "error",
						"error": map[string]interface{}{
							"type":    "api_error",
							"message": errData,
						},
					})
					w.Write([]byte("event: error\ndata: "))
					w.Write(errJSON)
					w.Write([]byte("\n\n"))
					// 发送 message_stop 终止 SSE 流，防止客户端等待超时
					w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
					flusher.Flush()
					return
				}
			}
			// 累积 usage（用于 -v 日志）
			usage := q.extractUsage(trimSSEDataPrefix(line))
			if usage != nil {
				lastUsage = usage
			}
			writeLine := line
			if aliasHit && aliasModel != "" {
				writeLine = echoAliasInStreamLine(line, aliasModel)
			}
			// 按 SSE 标准统一添加 data: 前缀（Provider 输出格式不一致：
			// OpenAIClient 输出纯 JSON，AnthropicClient 输出带 data: 前缀）
			writeLineStr := string(writeLine)
			if !strings.HasPrefix(writeLineStr, "data: ") &&
				!strings.HasPrefix(writeLineStr, "event: ") &&
				!strings.HasPrefix(writeLineStr, "id: ") &&
				!strings.HasPrefix(writeLineStr, "retry: ") &&
				len(writeLineStr) > 0 && writeLineStr[0] != ':' {
				if writeLineStr[0] == '{' || writeLineStr[0] == '[' {
					writeLine = append([]byte("data: "), writeLine...)
				}
			}
			if _, err := w.Write(writeLine); err != nil {
				log.Printf("[passthrough] stream write error: %s=%s url=%s err=%v",
					aliasModel, realModel, q.proxyBaseURL, err)
				return
			}
			w.Write([]byte("\n\n")) // SSE 协议要求空行分隔事件
			flusher.Flush()
		case <-heartbeat.C:
			streamHeartbeatCount++
			w.Write(heartbeatEvent)
			flusher.Flush()
			if q.verboseLevel >= 2 {
				log.Printf("[heartbeat] stream sent #%d → %v", streamHeartbeatCount, time.Since(streamHeartbeatStart))
			}
		}
	}
streamDone:

	vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
	vctx.upstreamReq = body
	q.logRequest(vctx, startTime, http.StatusOK, lastUsage, nil)
}

// sendSSEErrorFromUpstream 将上游错误转为合法的 Anthropic SSE error 事件并发送
// 解析 provider 错误格式 "HTTP %d: <body>"，提取上游错误 JSON 中的 type/message 字段
// 如果上游返回的是合法错误 JSON（如 {"type":"error","error":{...}}），直接透传
// 否则包装为通用错误格式
// 错误事件后发送 message_stop 以正确终止 SSE 流
// @AI_GUARD: SSE_ERROR_TRANSPARENCY - 上游错误必须透明传递，不丢失信息
// sendUpstreamErrorAsJSON 上游错误时返回 JSON 错误响应（非 SSE）
// 用于非流式调上游的上层函数（如 handlePassthroughNonStreamAsSSE），
// 此时 WriteHeader 尚未调用，可以设置正确的 HTTP 状态码
// @AI_GUARD: UPSTREAM_ERROR_JSON - 上游错误 JSON 响应，必须先调用上游再调此函数
// @CONSTRAINT: 必须在 WriteHeader 之前调用，否则状态码无法修改
//   - 解析上游错误格式 "HTTP %d: <json_body>"，提取状态码和错误体
//   - 标准格式 {"type":"error","error":{...}} 直接透传
//   - 非标准格式包装为 {"type":"error","error":{"type":"api_error","message":"..."}}
func sendUpstreamErrorAsJSON(w http.ResponseWriter, err error) {
	errStr := err.Error()
	statusCode := http.StatusBadGateway
	errorType := "api_error"
	errorMessage := errStr

	// provider 错误格式：HTTP %d: <json_body>
	if idx := strings.IndexByte(errStr, ':'); idx > 0 && strings.HasPrefix(errStr[:idx], "HTTP ") {
		statusStr := errStr[5:idx]
		if sc, parseErr := strconv.Atoi(statusStr); parseErr == nil && sc >= 400 {
			statusCode = sc
		}
		bodyStr := strings.TrimSpace(errStr[idx+1:])
		var upstreamErr map[string]interface{}
		if json.Unmarshal([]byte(bodyStr), &upstreamErr) == nil {
			if errType, ok := upstreamErr["type"].(string); ok && errType == "error" {
				// 标准格式，直接透传
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(statusCode)
				errJSON, _ := json.Marshal(upstreamErr)
				w.Write(errJSON)
				return
			}
			if errObj, ok := upstreamErr["error"].(map[string]interface{}); ok {
				if t, ok := errObj["type"].(string); ok && t != "" {
					errorType = t
				}
				if m, ok := errObj["message"].(string); ok && m != "" {
					errorMessage = m
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	resp := map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    errorType,
			"message": errorMessage,
		},
	}
	json.NewEncoder(w).Encode(resp)
}

func sendSSEErrorFromUpstream(w http.ResponseWriter, flusher http.Flusher, err error) {
	errStr := err.Error()
	errorType := "api_error"
	errorMessage := errStr

	// provider 错误格式：HTTP %d: <json_body>
	// 尝试从 "HTTP XXX: " 后提取上游错误 JSON
	if idx := strings.IndexByte(errStr, ':'); idx > 0 && strings.HasPrefix(errStr[:idx], "HTTP ") {
		bodyStr := strings.TrimSpace(errStr[idx+1:])
		var upstreamErr map[string]interface{}
		if json.Unmarshal([]byte(bodyStr), &upstreamErr) == nil {
			// 检查是否是标准错误格式 {"type":"error","error":{...}}
			if errType, ok := upstreamErr["type"].(string); ok && errType == "error" {
				// 直接透传上游错误体
				errJSON, _ := json.Marshal(upstreamErr)
				w.Write([]byte("event: error\ndata: "))
				w.Write(errJSON)
				w.Write([]byte("\n\n"))
				// 发送 message_stop 终止 SSE 流
				w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
				flusher.Flush()
				return
			}
			// 如果是其他错误格式，提取 error.message
			if errObj, ok := upstreamErr["error"].(map[string]interface{}); ok {
				if t, ok := errObj["type"].(string); ok && t != "" {
					errorType = t
				}
				if m, ok := errObj["message"].(string); ok && m != "" {
					errorMessage = m
				}
			}
		}
	}

	sendSSEErrorBody(w, flusher, errorType, errorMessage)
}

// sendSSEErrorBody 发送标准 Anthropic SSE error 事件 + message_stop
// @AI_GUARD: SSE_ERROR_BODY - 所有 SSE 错误出口必须统一使用此函数
// @CONSTRAINT: 错误事件格式：event: error\ndata: {"type":"error","error":{"type":"...","message":"..."}}\n\n
//   - 必须发送 message_stop 终止 SSE 流（否则客户端等待超时）
func sendSSEErrorBody(w http.ResponseWriter, flusher http.Flusher, errorType, errorMessage string) {
	errJSON, _ := json.Marshal(map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    errorType,
			"message": errorMessage,
		},
	})
	w.Write([]byte("event: error\ndata: "))
	w.Write(errJSON)
	w.Write([]byte("\n\n"))
	// 发送 message_stop 终止 SSE 流
	w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	flusher.Flush()
}

// probeStreamPrefer 后台异步探测 SSE 速度，完成后写入 streamPrefer 偏好
// 在首次请求用非流式响应后调用，不影响当前请求
func (q *QuickGateway) probeStreamPrefer(p provider.Provider, callInfo *schema.ProviderInfo, realModel string, nonStreamTime time.Duration) {
	// 构建最小探测请求（Anthropic 格式）
	probeBody := json.RawMessage(fmt.Sprintf(
		`{"model":"%s","messages":[{"role":"user","content":"ping"}],"stream":true,"max_tokens":1}`,
		realModel))

	probeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	start := time.Now()
	lines, _, err := p.CallStream(probeCtx, probeBody, callInfo)
	if err != nil {
		log.Printf("[probe] SSE probe failed: %s err=%v", realModel, err)
		return
	}

	// 等待第一个有效数据行（跳过 headers 元数据）
	for line := range lines {
		var meta map[string]any
		if json.Unmarshal(line, &meta) == nil {
			if meta["_type"] == "headers" {
				continue
			}
		}
		break
	}

	sseTime := time.Since(start)
	preferNonStream := sseTime >= nonStreamTime
	log.Printf("[probe] SSE probe: %s non_stream=%v sse=%v prefer_nonstream=%v",
		realModel, nonStreamTime, sseTime, preferNonStream)

	q.streamPreferMu.Lock()
	if q.streamPrefer == nil {
		q.streamPrefer = make(map[string]bool)
	}
	q.streamPrefer[q.proxyBaseURL] = preferNonStream
	q.streamPreferMu.Unlock()
}

// @AI_GUARD: NONSTREAM_AS_SSE - 非流式响应拆解为 SSE 事件流，修改前必须验证所有入站协议
// @CONSTRAINT: 自动检测响应格式（Anthropic/OpenAI/Gemini/Responses），每种协议的拆分逻辑不同
//   - Anthropic: 每个 content 块拆分为一个 SSE 事件
//   - OpenAI CC: choices[0].message.content 拆分为一个 content_block_delta
//   - Gemini: candidates[0].content.parts 拆分为一个 content_block_delta
//   - Responses: output 拆分为一个 content_block_delta
//   - 所有格式的输出必须用 data: 前缀 + \n\n 双换行
//
// @RELATED: handleNonStreamResponseAsSSE, handlePassthroughNonStreamAsSSE
// @REASON: 历史血泪教训 - 各种协议的非流式响应结构不同，修改拆解逻辑需同步所有格式
// writeNonStreamAsSSE 将非流式完整响应 JSON 拆解为对应协议的 SSE 多事件流写入 w。
// 自动检测响应格式（Anthropic / OpenAI ChatCompletion / Gemini / OpenAI Responses），
// 返回从响应中提取的 usage（用于日志）。
func (q *QuickGateway) writeNonStreamAsSSE(w http.ResponseWriter, flusher http.Flusher, respBody []byte, effectiveModel string) *schema.InternalUsage {
	usage := q.extractUsage(respBody)

	var respMap map[string]interface{}
	if err := json.Unmarshal(respBody, &respMap); err != nil {
		var compactBuf bytes.Buffer
		if err := json.Compact(&compactBuf, respBody); err != nil {
			compactBuf.Reset()
			compactBuf.Write(respBody)
		}
		w.Write([]byte("data: "))
		w.Write(compactBuf.Bytes())
		w.Write([]byte("\n\n"))
		flusher.Flush()
		return usage
	}

	// 检测响应格式：优先 Anthropic Messages，其次 OpenAI ChatCompletion，再 Gemini，再 OpenAI Responses
	if contentRaw, ok := respMap["content"]; ok {
		if contentArr, ok := contentRaw.([]interface{}); ok {
			// ── Anthropic Messages 格式 ──
			msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
			stopReason := "end_turn"
			if sr, ok := respMap["stop_reason"].(string); ok && sr != "" {
				stopReason = sr
			}
			// 按 Anthropic 标准构造 message_start 事件：
			// - type: "message" 必填
			// - stop_reason: null（流式开始时为 null，结束时在 message_delta 给出）
			// - usage: {input_tokens, output_tokens:1}（必须是对象，不能为 null）
			inputTokens := 0
			if usage != nil {
				inputTokens = usage.PromptTokens
			}
			messageStart := fmt.Sprintf(
				`{"type":"message_start","message":{"id":"%s","type":"message","role":"assistant","content":[],"model":"%s","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":1}}}`,
				msgID, effectiveModel, inputTokens)
			if !writeSSE(w, []byte("data: "+messageStart+"\n\n")) {
				return usage
			}

			for idx, blockAny := range contentArr {
				blockMap, ok := blockAny.(map[string]interface{})
				if !ok {
					continue
				}
				blockType := "text"
				if bt, ok := blockMap["type"].(string); ok && bt != "" {
					blockType = bt
				}

				// content_block_start（按 Anthropic 标准补 citations:[]）
				blockStartMap := map[string]interface{}{"type": blockType}
				if blockType == "text" {
					blockStartMap["text"] = ""
					blockStartMap["citations"] = []interface{}{}
				}
				blockStart := map[string]interface{}{
					"type":          "content_block_start",
					"index":         idx,
					"content_block": blockStartMap,
				}
				blockStartJSON, err := json.Marshal(blockStart)
				if err != nil {
					continue
				}
				if !writeSSE(w, []byte("data: "+string(blockStartJSON)+"\n\n")) {
					return usage
				}

				// content_block_delta — 按 block type 区分 delta 类型和字段名
				deltaJSON, err := func() ([]byte, error) {
					switch blockType {
					case "text":
						var txt string
						if t, ok := blockMap["text"].(string); ok {
							txt = t
						}
						return json.Marshal(map[string]interface{}{
							"type":  "content_block_delta",
							"index": idx,
							"delta": map[string]interface{}{
								"type": "text_delta",
								"text": txt,
							},
						})
					case "thinking":
						var thinking string
						if t, ok := blockMap["thinking"].(string); ok {
							thinking = t
						}
						return json.Marshal(map[string]interface{}{
							"type":  "content_block_delta",
							"index": idx,
							"delta": map[string]interface{}{
								"type":     "thinking_delta",
								"thinking": thinking,
							},
						})
					default:
						return nil, nil
					}
				}()
				if err != nil || len(deltaJSON) == 0 {
					continue
				}
				if !writeSSE(w, []byte("data: "+string(deltaJSON)+"\n\n")) {
					return usage
				}

				// content_block_stop
				blockStopJSON, err := json.Marshal(map[string]interface{}{
					"type":  "content_block_stop",
					"index": idx,
				})
				if err != nil {
					continue
				}
				if !writeSSE(w, []byte("data: "+string(blockStopJSON)+"\n\n")) {
					return usage
				}
			}

			// message_delta: 按 Anthropic 标准，usage 仅含 output_tokens（input_tokens 已在 message_start 给出）
			// delta 必须包含 stop_sequence:null
			if usage != nil {
				messageDelta := fmt.Sprintf(
					`{"type":"message_delta","delta":{"stop_reason":"%s","stop_sequence":null},"usage":{"output_tokens":%d}}`,
					stopReason, usage.CompletionTokens)
				if !writeSSE(w, []byte("data: "+messageDelta+"\n\n")) {
					return usage
				}
			}
			// message_stop
			if !writeSSE(w, []byte("data: {\"type\":\"message_stop\"}\n\n")) {
				return usage
			}
			flusher.Flush()
		} else {
			// content 字段存在但不是数组 → 降级为裸 JSON 单事件
			w.Write([]byte("data: "))
			w.Write(respBody)
			w.Write([]byte("\n\n"))
			flusher.Flush()
		}
	} else if choices, ok := respMap["choices"]; ok {
		// ── OpenAI ChatCompletion 格式 ──
		choicesArr, ok := choices.([]interface{})
		if !ok {
			choicesArr = []interface{}{}
		}
		for _, choiceAny := range choicesArr {
			choiceMap, ok := choiceAny.(map[string]interface{})
			if !ok {
				continue
			}
			msgMap, ok := choiceMap["message"].(map[string]interface{})
			if !ok {
				msgMap = map[string]interface{}{"role": "assistant", "content": ""}
			}
			if _, hasModel := msgMap["model"]; !hasModel {
				msgMap["model"] = effectiveModel
			}
			chunk := map[string]interface{}{
				"id":      fmt.Sprintf("chatcmpl_%d", time.Now().UnixNano()),
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   effectiveModel,
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"delta": map[string]interface{}{
							"role":    "assistant",
							"content": msgMap["content"],
						},
						"finish_reason": choiceMap["finish_reason"],
					},
				},
			}
			chunkJSON, err := json.Marshal(chunk)
			if err != nil {
				continue
			}
			if !writeSSE(w, []byte("data: "+string(chunkJSON)+"\n\n")) {
				return usage
			}
		}
		if !writeSSE(w, []byte("data: [DONE]\n\n")) {
			return usage
		}
		flusher.Flush()
	} else if candidates, ok := respMap["candidates"]; ok {
		// ── Gemini 格式 ──
		candArr, ok := candidates.([]interface{})
		if !ok {
			candArr = []interface{}{}
		}
		for _, candAny := range candArr {
			candMap, ok := candAny.(map[string]interface{})
			if !ok {
				continue
			}
			chunk := map[string]interface{}{
				"candidates":    []interface{}{candMap},
				"usageMetadata": candMap["usageMetadata"],
			}
			chunkJSON, err := json.Marshal(chunk)
			if err != nil {
				continue
			}
			if !writeSSE(w, []byte("data: "+string(chunkJSON)+"\n\n")) {
				return usage
			}
		}
		flusher.Flush()
	} else if output, ok := respMap["output"]; ok {
		// ── OpenAI Responses 格式 ──
		outputArr, ok := output.([]interface{})
		if !ok {
			outputArr = []interface{}{}
		}
		for _, itemAny := range outputArr {
			itemMap, ok := itemAny.(map[string]interface{})
			if !ok {
				continue
			}
			chunk := map[string]interface{}{
				"id":       fmt.Sprintf("resp_%d", time.Now().UnixNano()),
				"object":   "response.text_delta",
				"response": map[string]interface{}{"id": itemMap["id"], "model": effectiveModel},
				"output":   []interface{}{itemMap},
				"usage":    itemMap["usage"],
			}
			chunkJSON, err := json.Marshal(chunk)
			if err != nil {
				continue
			}
			if !writeSSE(w, []byte("data: "+string(chunkJSON)+"\n\n")) {
				return usage
			}
		}
		if !writeSSE(w, []byte("data: [DONE]\n\n")) {
			return usage
		}
		flusher.Flush()
	} else {
		// 未知格式 → 降级为裸 JSON 单事件
		w.Write([]byte("data: "))
		w.Write(respBody)
		w.Write([]byte("\n\n"))
		flusher.Flush()
	}

	return usage
}

// @AI_GUARD: PASSTHROUGH_NONSTREAM_AS_SSE - 透传路径非流式→SSE 包装，调用 writeNonStreamAsSSE
// @CONSTRAINT: 上游非流式请求期间发送心跳，close(callDone) 必须在 <-callFinished 之前（顺序不可颠倒）
//   - 必须等待 callFinished 确保心跳 goroutine 完全退出后再写 SSE 响应，否则并发写 ResponseWriter 造成 SSE 数据损坏
//   - 响应体必须经过 stripThinkingContentBlocks 过滤
//   - 拆解为 SSE 事件流通过 writeNonStreamAsSSE 完成
//
// @RELATED: CALLDONE_CALLFINISHED (同步模式), writeNonStreamAsSSE, handleNonStreamResponseAsSSE (翻译路径对应)
// @REASON: 历史血泪教训 - 遗漏 <-callFinished 导致心跳与 SSE 响应并发写入 http.ResponseWriter，Claude Code 解析到损坏 SSE 后报 "API returned an empty or malformed response (HTTP 200)"
// handlePassthroughNonStreamAsSSE 非流式调上游，包装成 SSE 返回
// @AI_GUARD: PASSTHROUGH_NONSTREAM_AS_SSE - 透传路径非流式→SSE 包装，调用 writeNonStreamAsSSE
// @CONSTRAINT: 先调用上游再设置 HTTP 状态码，避免心跳 goroutine 触发 WriteHeader(200) 后无法修改
//   - 上游调用在前，成功后才设置 SSE 头并启动心跳
//   - 上游错误时直接返回 JSON 错误（非 SSE），设置正确的 HTTP 状态码
//   - 成功路径：WriteHeader(200) → 心跳 → writeNonStreamAsSSE
//   - 必须等待 callFinished 确保心跳 goroutine 完全退出后再写 SSE 响应
//
// @RELATED: CALLDONE_CALLFINISHED, writeNonStreamAsSSE, handleNonStreamResponseAsSSE
// @REASON: 历史血泪教训 - 心跳 goroutine 先触发 WriteHeader(200)，上游返回 400 时无法修改为正确状态码，
//
//	Claude Code 看到 HTTP 200 + event:error → 解析为 "API returned an empty or malformed response (HTTP 200)"
func (q *QuickGateway) handlePassthroughNonStreamAsSSE(p provider.Provider, ctx context.Context, w http.ResponseWriter,
	r *http.Request, realModel string, aliasModel string, aliasHit bool, startTime time.Time, body []byte) {

	if q.verboseLevel >= 2 {
		handlerStart := time.Now()
		log.Printf("[handler] passthrough-nonstream-as-sse: model=%s, alias=%s", realModel, aliasModel)
		defer func() {
			log.Printf("[handler] passthrough-nonstream-as-sse → %v (alias=%s, model=%s)", time.Since(handlerStart), aliasModel, realModel)
		}()
	}
	callInfo := makeQuickPassthroughInfo(q.info, realModel)

	// 去掉 stream 标记
	nsBody := quickRemoveStreamFlag(body)
	if aliasHit && aliasModel != "" {
		nsBody = quickReplaceModelInBody(nsBody, aliasModel, realModel)
	}

	// 先调用上游，再决定 HTTP 状态码
	// 非流式上游调用响应时间通常很短（< 5s），不需要心跳
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(q.timeout)*time.Second)
	upstreamStart := time.Now()
	respBody, _, err := p.Call(callCtx, nsBody, callInfo)
	cancel()
	if q.verboseLevel >= 2 {
		log.Printf("[upstream] Call %s → %v", realModel, time.Since(upstreamStart))
	}
	if err != nil {
		// 上游返回错误，直接返回 JSON 错误响应（非 SSE）
		// 解析上游错误 JSON，提取 type/message 字段
		log.Printf("[passthrough] nonstream-as-sse error: %s=%s url=%s err=%v",
			aliasModel, realModel, q.proxyBaseURL, err)
		// 客户端期望 SSE 格式，必须发 SSE 错误而非 JSON
		flusher, ok := w.(http.Flusher)
		if !ok {
			sendUpstreamErrorAsJSON(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		if q.verboseLevel >= 2 {
			log.Printf("[sse-error] passthrough-nonstream-as-sse → SSE error event (was JSON)")
		}
		sendSSEErrorFromUpstream(w, flusher, err)
		flusher.Flush()
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		q.sendError(w, http.StatusInternalServerError, "streaming not supported", "server does not support streaming")
		return
	}

	// 上游成功，设置 SSE 头
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// 启动心跳，防止客户端在 writeNonStreamAsSSE 期间超时
	callDone, callFinished := StartSSEHeartbeat(w, flusher, r.Context(), q.verboseLevel)

	// 回显客户端模型名（嵌入到 SSE 事件的 model 字段）
	effectiveModel := realModel
	if aliasHit && aliasModel != "" {
		effectiveModel = aliasModel
	}

	// 过滤 thinking 内容块（同 handlePassthroughNonStream）
	respBody = stripThinkingContentBlocks(respBody)

	usage := q.writeNonStreamAsSSE(w, flusher, respBody, effectiveModel)

	close(callDone) // 停止心跳
	<-callFinished  // 等待心跳 goroutine 退出

	vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
	vctx.upstreamReq = nsBody
	vctx.upstreamResp = respBody
	q.logRequest(vctx, startTime, http.StatusOK, usage, nil)
}

// @AI_GUARD: NONSTREAM_RESPONSE - 翻译路径非流式→非流式 JSON 返回
// @CONSTRAINT: 翻译管道：TranslateToProvider → Call → TranslateFromProvider → TranslateResponse → JSON
//   - 先 WriteHeader(200) + Flush() 启用 chunked transfer
//   - 非流式请求使用独立 http.Client，与 SSE 连接池隔离
//
// @RELATED: handleNonStreamResponseAsSSE (翻译路径非流式→SSE 包装)
// handleNonStreamResponse 非流式响应
func (q *QuickGateway) handleNonStreamResponse(p provider.Provider, ctx context.Context, w http.ResponseWriter,
	downstreamReq json.RawMessage, providerTranslator any,
	ingressTranslator translator.CombinedTranslator, internalReq *schema.InternalRequest,
	startTime time.Time) {

	if q.verboseLevel >= 2 {
		handlerStart := time.Now()
		log.Printf("[handler] translation-nonstream: model=%s, stream=%v", internalReq.Model, internalReq.Stream)
		defer func() {
			log.Printf("[handler] translation-nonstream → %v (model=%s)", time.Since(handlerStart), internalReq.Model)
		}()
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(q.timeout)*time.Second)
	defer cancel()

	upstreamStart := time.Now()
	resp, headers, err := p.Call(callCtx, downstreamReq, q.info)
	if q.verboseLevel >= 2 {
		log.Printf("[upstream] Call %s → %v", internalReq.Model, time.Since(upstreamStart))
	}
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

	// ── 若命中别名映射，将响应中的模型名回显为客户端原始模型名 ──
	if internalReq != nil && internalReq.AliasModel != "" {
		internalResp.Model = internalReq.AliasModel
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
	vctx.upstreamResp = resp
	vctx.outgoingBody = outgoingResp
	q.logRequest(vctx, startTime, http.StatusOK, usage, nil)
}

// @AI_GUARD: NONSTREAM_RESPONSE_AS_SSE - 翻译路径非流式→SSE 包装
// @CONSTRAINT: 先调用上游再设置 HTTP 状态码，避免心跳 goroutine 触发 WriteHeader(200) 后无法修改
//   - 上游调用在前，成功后才设置 SSE 头并启动心跳
//   - 上游错误时直接返回 JSON 错误（非 SSE），设置正确的 HTTP 状态码
//   - 成功路径：TranslateFromProvider → TranslateResponse → WriteHeader(200) → 心跳 → writeNonStreamAsSSE
//   - 必须等待 callFinished 确保心跳 goroutine 完全退出后再写 SSE 响应
//
// @RELATED: CALLDONE_CALLFINISHED (同步模式), writeNonStreamAsSSE, handlePassthroughNonStreamAsSSE (透传路径对应)
// @REASON: 历史血泪教训 - 遗漏 <-callFinished 导致心跳与 SSE 响应并发写入 http.ResponseWriter，Claude Code 解析到损坏 SSE 后报 "API returned an empty or malformed response (HTTP 200)"
func (q *QuickGateway) handleNonStreamResponseAsSSE(p provider.Provider, ctx context.Context, w http.ResponseWriter,
	downstreamReq json.RawMessage, providerTranslator any,
	ingressTranslator translator.CombinedTranslator, internalReq *schema.InternalRequest,
	startTime time.Time) {

	if q.verboseLevel >= 2 {
		handlerStart := time.Now()
		log.Printf("[handler] translation-nonstream-as-sse: model=%s, stream=%v", internalReq.Model, internalReq.Stream)
		defer func() {
			log.Printf("[handler] translation-nonstream-as-sse → %v (model=%s)", time.Since(handlerStart), internalReq.Model)
		}()
	}
	// 先调用上游，再决定 HTTP 状态码
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(q.timeout)*time.Second)
	upstreamStart := time.Now()
	resp, headers, err := p.Call(callCtx, downstreamReq, q.info)
	cancel()
	if q.verboseLevel >= 2 {
		log.Printf("[upstream] Call %s → %v", internalReq.Model, time.Since(upstreamStart))
	}
	if err != nil {
		// 客户端期望 SSE 格式，必须发 SSE 错误而非 JSON
		flusher, ok := w.(http.Flusher)
		if !ok {
			sendUpstreamErrorAsJSON(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		if q.verboseLevel >= 2 {
			log.Printf("[sse-error] translation-nonstream-as-sse → SSE error event (was JSON)")
		}
		sendSSEErrorFromUpstream(w, flusher, err)
		flusher.Flush()
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		q.sendError(w, http.StatusInternalServerError, "streaming not supported", "server does not support streaming")
		return
	}

	// 上游成功，设置 SSE 头
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// 启动心跳，防止客户端在 writeNonStreamAsSSE 期间超时
	callDone, callFinished := StartSSEHeartbeat(w, flusher, ctx, q.verboseLevel)

	// ── 出 Provider 翻译：Provider 响应 → InternalResponse ──
	var internalResp *schema.InternalResponse
	if providerTranslator != nil {
		pt := providerTranslator.(interface {
			TranslateFromProvider(json.RawMessage) (*schema.InternalResponse, error)
		})
		internalResp, err = pt.TranslateFromProvider(resp)
		if err != nil {
			close(callDone) // 停止心跳
			<-callFinished  // 等待心跳 goroutine 退出
			// 翻译错误必须用标准 Anthropic SSE error 格式
			if q.verboseLevel >= 2 {
				log.Printf("[sse-error] translation-nonstream-as-sse → TranslateFromProvider failed: %v", err)
			}
			sendSSEErrorFromUpstream(w, flusher, err)
			flusher.Flush()
			return
		}
	} else {
		var ccResp chatcompletion.ChatCompletionResponse
		if err := json.Unmarshal(resp, &ccResp); err != nil {
			close(callDone) // 停止心跳
			<-callFinished  // 等待心跳 goroutine 退出
			// 解析错误必须用标准 Anthropic SSE error 格式
			if q.verboseLevel >= 2 {
				log.Printf("[sse-error] translation-nonstream-as-sse → json.Unmarshal failed: %v", err)
			}
			sendSSEErrorFromUpstream(w, flusher, fmt.Errorf("parse error: %w", err))
			return
		}
		internalResp = chatCompletionToInternal(&ccResp)
	}

	// 回显客户端模型名
	if internalReq != nil && internalReq.AliasModel != "" {
		internalResp.Model = internalReq.AliasModel
	}

	// ── 出站翻译：InternalResponse → 入站协议格式 ──
	outgoingResp, err := ingressTranslator.TranslateResponse(internalResp)
	if err != nil {
		// @AI_GUARD: STREAM_TRANSLATE_ERROR - 翻译错误必须用标准 Anthropic SSE error 格式
		close(callDone) // 停止心跳
		<-callFinished  // 等待心跳 goroutine 退出
		if q.verboseLevel >= 2 {
			log.Printf("[sse-error] translation-nonstream-as-sse → TranslateResponse failed: %v", err)
		}
		sendSSEErrorBody(w, flusher, "internal_error", fmt.Sprintf("encode error: %v", err))
		flusher.Flush()
		return
	}

	// 透传下游响应头
	for k, v := range headers {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}

	// 将翻译后的入站协议响应拆解为 SSE 事件流
	effectiveModel := internalResp.Model
	usage := q.writeNonStreamAsSSE(w, flusher, outgoingResp, effectiveModel)

	close(callDone) // 停止心跳
	<-callFinished  // 等待心跳 goroutine 退出

	vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
	vctx.upstreamResp = resp
	vctx.outgoingBody = outgoingResp
	q.logRequest(vctx, startTime, http.StatusOK, usage, nil)
}

// @AI_GUARD: HANDLE_STREAM_REQUEST - 翻译路径流式处理，核心约束密集
// @CONSTRAINT:
//  1. TranslateStreamEvent 类型断言签名必须为 json.RawMessage（Gemini/Anthropic 一致，Responses 除外）
//  2. 入站翻译器 TranslateStream 负责最终 SSE 事件序列（Anthropic 必须 message_start→...→message_stop）
//  3. callDone/callFinished 同步不可移除
//  4. 上游 SSE 行必须剥离 "data: " 前缀后再传入翻译器
//
// @RELATED: TranslateStreamEvent (各翻译器), TranslateStream (入站翻译器)
// @REASON: 历史血泪教训 - 修改 SSE 事件格式后未同步 TranslateStream，导致 Kimi 解析失败
// handleStreamRequest 流式请求
func (q *QuickGateway) handleStreamRequest(p provider.Provider, ctx context.Context, w http.ResponseWriter,
	downstreamReq json.RawMessage, providerTranslator any,
	ingressTranslator translator.CombinedTranslator, internalReq *schema.InternalRequest,
	startTime time.Time) {

	if q.verboseLevel >= 2 {
		handlerStart := time.Now()
		log.Printf("[handler] translation-stream: model=%s, stream=%v", internalReq.Model, internalReq.Stream)
		defer func() {
			log.Printf("[handler] translation-stream → %v (model=%s)", time.Since(handlerStart), internalReq.Model)
		}()
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		q.sendError(w, http.StatusInternalServerError, "stream", "server does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // 立即发送响应头，防止客户端在等待上游首个响应时超时断开

	// 在 CallStream 阻塞等待上游首个响应期间发送 SSE 心跳
	callDone, callFinished := StartSSEHeartbeat(w, flusher, ctx, q.verboseLevel)

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(q.timeout)*time.Second)
	defer cancel()

	upstreamStart := time.Now()
	lines, _, err := p.CallStream(callCtx, downstreamReq, q.info)
	if q.verboseLevel >= 2 {
		log.Printf("[upstream] CallStream %s → %v", internalReq.Model, time.Since(upstreamStart))
	}
	close(callDone) // 停止 CallStream 期间的心跳
	<-callFinished  // 等待心跳 goroutine 退出，防止并发写

	if err != nil {
		// SSE 头已设置，不能调用 sendError，直接写 SSE 错误事件
		log.Printf("[stream] upstream stream error: %s err=%v", q.proxyBaseURL, err)
		sendSSEErrorFromUpstream(w, flusher, fmt.Errorf("stream error: %w", err))
		return
	}

	// 别名模型回显：若命中别名映射，将 InternalStreamChunk 中的 Model 覆盖为客户端原始模型名
	aliasModel := ""
	if internalReq != nil {
		aliasModel = internalReq.AliasModel
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
			// OpenAIClient.CallStream 发送的 SSE 行包含 "data: " 前缀，
			// 需要在传入翻译器前剥离（与下方 OpenAI 路径一致）
			payload := strings.TrimPrefix(string(line), "data: ")
			if payload == "[DONE]" {
				continue
			}

			if providerTranslator != nil {
				// @AI_GUARD: TRANSLATE_STREAM_EVENT_SIGNATURE - 类型断言签名必须为 json.RawMessage
				// @CONSTRAINT: 所有协议的 TranslateStreamEvent 必须接受 json.RawMessage 参数
				//   - Anthropic/Gemini: 签名一致 ✓
				//   - Responses: 签名为 *StreamEvent，类型断言失败 → 走 else 分支（OpenAI 兼容路径）
				//   - 新增协议翻译器必须实现 TranslateStreamEvent(json.RawMessage) 签名
				// @RELATED: anthropic/translator.go:792, gemini/translator.go:771, responses/translator.go:721
				// @REASON: 历史血泪教训 - Responses 翻译器签名不一致导致其 TranslateStreamEvent 成为死代码
				pte := providerTranslator.(interface {
					TranslateStreamEvent(json.RawMessage) *schema.InternalStreamEvent
				})
				event := pte.TranslateStreamEvent(json.RawMessage(payload))
				if event != nil {
					if event.Data != nil && event.Data.Usage != nil {
						accumulatedUsage = event.Data.Usage
					}
					if aliasModel != "" && event.Data != nil {
						event.Data.Model = aliasModel
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
				modelName := ccChunk.Model
				if aliasModel != "" {
					modelName = aliasModel
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
				if ccChunk.Usage != nil {
					accumulatedUsage = mapInternalUsage(ccChunk.Usage)
				}
			}
		}
	}()

	// @AI_GUARD: TRANSLATE_STREAM_OUTPUT - 入站翻译器 TranslateStream 负责最终 SSE 事件序列
	// @CONSTRAINT: Anthropic 翻译器必须严格遵循事件序列：
	//   message_start → content_block_start → content_block_delta* → content_block_stop → message_delta → message_stop
	//   - 每个事件后必须跟 \n\n 双换行
	//   - 所有数据行必须带 data: 前缀
	// @RELATED: anthropic/translator.go:344 TranslateStream
	// @REASON: 历史血泪教训 - 事件序列不完整导致 Kimi 解析失败，修复多次才稳定
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

// @AI_GUARD: STREAM_REQUEST_AS_NONSTREAM - 翻译路径上游流式→非流式 JSON 返回
// @CONSTRAINT: 收集所有 SSE 事件 → 组装 InternalResponse → 翻译为入站协议 JSON
//   - 先 WriteHeader(200) + Flush() 启用 chunked transfer，防止客户端等待超时
//   - usage 需要从最后一个有 usage 的 SSE 事件提取
//
// @RELATED: handleStreamRequest (反向路径)
// @REASON: 历史血泪教训 - 未先写响应头导致客户端等待上游 SSE 收集时超时 ECONNRESET
// handleStreamRequestAsNonStream 翻译路径：
// 调用上游流式 API，收集所有 SSE 事件经翻译管道组装为完整 InternalResponse，
// 再翻译为入站协议格式的 JSON 返回（客户端发的是非流式请求）。
func (q *QuickGateway) handleStreamRequestAsNonStream(p provider.Provider, ctx context.Context, w http.ResponseWriter,
	downstreamReq json.RawMessage, providerTranslator any,
	ingressTranslator translator.CombinedTranslator, internalReq *schema.InternalRequest,
	startTime time.Time) {

	if q.verboseLevel >= 2 {
		handlerStart := time.Now()
		log.Printf("[handler] translation-stream-as-nonstream: model=%s", internalReq.Model)
		defer func() {
			log.Printf("[handler] translation-stream-as-nonstream → %v (model=%s)", time.Since(handlerStart), internalReq.Model)
		}()
	}

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(q.timeout)*time.Second)
	defer cancel()

	upstreamStart := time.Now()
	lines, _, err := p.CallStream(callCtx, downstreamReq, q.info)
	if q.verboseLevel >= 2 {
		log.Printf("[upstream] CallStream %s → %v", internalReq.Model, time.Since(upstreamStart))
	}
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

	// 先写响应头并 flush，防止长响应收集期间客户端读超时（ECONNRESET）
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// 收集所有流式事件，累积文本与推理内容
	var accumulatedContent strings.Builder
	var accumulatedReasoning strings.Builder
	var lastModel string
	var lastFinishReason string
	var lastUsage *schema.InternalUsage

	aliasModel := ""
	if internalReq != nil {
		aliasModel = internalReq.AliasModel
	}

	for line := range lines {
		select {
		case <-ctx.Done():
			return
		default:
		}
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
				q.sendError(w, int(status), "upstream_error", data)
				return
			}
		}

		payload := strings.TrimPrefix(string(line), "data: ")
		if payload == "[DONE]" {
			continue
		}

		if providerTranslator != nil {
			pte, ok := providerTranslator.(interface {
				TranslateStreamEvent(json.RawMessage) *schema.InternalStreamEvent
			})
			if !ok {
				continue
			}
			event := pte.TranslateStreamEvent(json.RawMessage(payload))
			if event != nil && event.Data != nil {
				if event.Data.Usage != nil {
					lastUsage = event.Data.Usage
				}
				lastModel = event.Data.Model
				for _, choice := range event.Data.Choices {
					if choice.FinishReason != "" {
						lastFinishReason = choice.FinishReason
					}
					if choice.Message.Content != nil {
						var contentStr string
						json.Unmarshal(choice.Message.Content, &contentStr)
						accumulatedContent.WriteString(contentStr)
					}
				}
			}
		} else {
			// OpenAI 兼容：直接解析 SSE delta 行
			var ccChunk chatcompletion.ChatCompletionStreamChunk
			if json.Unmarshal([]byte(payload), &ccChunk) != nil || len(ccChunk.Choices) == 0 {
				continue
			}
			choice := ccChunk.Choices[0]
			if choice.Delta.Content != "" {
				accumulatedContent.WriteString(choice.Delta.Content)
			}
			if choice.Delta.Reasoning != "" {
				accumulatedReasoning.WriteString(choice.Delta.Reasoning)
			}
			if choice.FinishReason != "" {
				lastFinishReason = choice.FinishReason
			}
			lastModel = ccChunk.Model
			if ccChunk.Usage != nil {
				lastUsage = mapInternalUsage(ccChunk.Usage)
			}
		}
	}

	// 组装完整 InternalResponse
	modelName := lastModel
	if aliasModel != "" {
		modelName = aliasModel
	}

	contentJSON, _ := json.Marshal(accumulatedContent.String())
	reasoning := accumulatedReasoning.String()
	internalResp := &schema.InternalResponse{
		ID:    fmt.Sprintf("chatcmpl_%d", time.Now().UnixNano()),
		Model: modelName,
		Choices: []schema.InternalChoice{{
			Index: 0,
			Message: schema.InternalMessage{
				Role:    schema.RoleAssistant,
				Content: contentJSON,
				ContentBlocks: func() []schema.InternalContentBlock {
					if reasoning != "" {
						return []schema.InternalContentBlock{{Type: "thinking", Thinking: reasoning}}
					}
					return nil
				}(),
			},
			FinishReason: lastFinishReason,
		}},
		Usage:  lastUsage,
		Object: "chat.completion",
	}

	// 翻译为入站协议格式
	outgoingResp, err := ingressTranslator.TranslateResponse(internalResp)
	if err != nil {
		q.sendError(w, http.StatusInternalServerError, "encode_response", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(outgoingResp)

	vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
	vctx.outgoingBody = outgoingResp
	q.logRequest(vctx, startTime, http.StatusOK, lastUsage, nil)
}

// translateToProvider 根据目标 Provider 类型选择翻译器并构建下游请求体
// providerType 来自调用方本地变量，避免读取共享 q.info.Version 造成数据竞争。
// @AI_GUARD: TRANSLATE_TO_PROVIDER - InternalRequest → 上游协议请求（Central Schema 出口）
// @CONSTRAINT: 根据 providerType 选择正确的翻译器并构建下游请求体
//   - 返回 (providerTranslator, downstreamReq) 供后续 handleStreamRequest/handleNonStreamResponse 使用
//   - 新增协议时必须在此 switch 中注册对应的翻译器
//
// @RELATED: all protocol/translator.go TranslateToProvider 方法
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
	// 上游请求始终使用 proxyKey
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

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		q.sendError(w, http.StatusInternalServerError, "read_body", err.Error())
		return
	}

	// ?simple=1: 返回纯文本模型列表（浏览器友好）
	if r.URL.Query().Get("simple") == "1" {
		var resp2 struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		json.Unmarshal(bodyBytes, &resp2)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		// 收集上游模型ID
		upstreamModels := make(map[string]bool)
		for _, m := range resp2.Data {
			upstreamModels[m.ID] = true
		}

		// 第一部分：上游真实模型
		fmt.Fprintln(w, "=== 上游模型 ===")
		for _, m := range resp2.Data {
			fmt.Fprintln(w, m.ID)
		}

		// 第二部分：别名映射表
		if q.aliasFile != nil && len(q.aliasFile.Entries()) > 0 {
			// 解析 @default 到上游第一个模型（用于显示）
			defaultDisplay := "@default"
			if len(resp2.Data) > 0 {
				defaultDisplay = resp2.Data[0].ID
			}
			fmt.Fprintln(w)
			fmt.Fprintln(w, "=== 别名映射 ===")
			for alias := range q.aliasFile.Entries() {
				if alias == "_default_" {
					continue
				}
				target, _ := q.aliasFile.Resolve(alias)
				if target == "@default" {
					target = defaultDisplay
				}
				fmt.Fprintf(w, "%s <-> %s\n", alias, target)
			}
		}
		return
	}

	// 解析上游响应，追加别名映射信息
	var upstreamResp map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &upstreamResp); err != nil {
		// 解析失败则原样返回
		w.WriteHeader(resp.StatusCode)
		w.Write(bodyBytes)
		return
	}

	// 若有别名映射，追加别名模型到 data 列表 + metadata.aliases
	if q.aliasFile != nil && len(q.aliasFile.Entries()) > 0 {
		entries := q.aliasFile.Entries()
		mapping := make(map[string]string, len(entries))
		for alias := range entries {
			if alias == "_default_" {
				continue
			}
			target, _ := q.aliasFile.Resolve(alias)
			mapping[alias] = target
		}
		// 将别名模型也加入 data 列表，让客户端能发现它们
		if dataArr, ok := upstreamResp["data"].([]interface{}); ok {
			existing := make(map[string]bool)
			for _, item := range dataArr {
				if m, ok := item.(map[string]interface{}); ok {
					if id, ok := m["id"].(string); ok {
						existing[id] = true
					}
				}
			}
			for alias := range entries {
				if alias == "_default_" {
					continue
				}
				if !existing[alias] {
					dataArr = append(dataArr, map[string]interface{}{
						"id":      alias,
						"object":  "model",
						"owner":   "proxy-alias",
						"aliased": true,
					})
				}
			}
			upstreamResp["data"] = dataArr
		}
		metadata := upstreamResp["metadata"]
		if metadata == nil {
			metadata = make(map[string]interface{})
			upstreamResp["metadata"] = metadata
		}
		if metaMap, ok := metadata.(map[string]interface{}); ok {
			metaMap["aliases"] = mapping
		}
	}

	// 透传上游响应头
	for k, v := range resp.Header {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	json.NewEncoder(w).Encode(upstreamResp)
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
// -vv 级别: 在 -v 基础上依次显示四向消息体（不截断）：
//  1. [Guest → 代理] 入站原始请求体（客户端假模型名）
//  2. [代理 → LLM]   最终发送给上游的请求体（真实模型名）
//  3. [LLM → 代理]   上游原始响应体（真实模型名）
//  4. [代理 → Guest] 最终发回客户端的响应体（回显客户端模型名）
func (q *QuickGateway) logRequest(vctx verboseCtx, startTime time.Time, status int, usage *schema.InternalUsage, _ []byte) {
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
		if len(vctx.ingressBody) > 0 {
			fmt.Printf("[Guest → 代理] 入站原始请求体:\n%s\n", formatJSON(vctx.ingressBody))
		}
		if len(vctx.upstreamReq) > 0 {
			fmt.Printf("[代理 → LLM] 上游请求体:\n%s\n", formatJSON(vctx.upstreamReq))
		}
		if len(vctx.upstreamResp) > 0 {
			fmt.Printf("[LLM → 代理] 上游原始响应体:\n%s\n", formatJSON(vctx.upstreamResp))
		}
		if len(vctx.outgoingBody) > 0 {
			fmt.Printf("[代理 → Guest] 出站响应体:\n%s\n", formatJSON(vctx.outgoingBody))
		}
	}
}

// trimSSEDataPrefix 去掉 SSE 行开头的 "data: " 前缀（如有），返回纯 JSON 内容
func trimSSEDataPrefix(line []byte) []byte {
	s := string(line)
	if rest, ok := strings.CutPrefix(s, "data: "); ok {
		return []byte(rest)
	}
	return line
}

// extractUsage 从响应 JSON 中提取 usage 信息。
// 兼容多种字段名：
//   - OpenAI/ChatCompletion: usage.prompt_tokens / completion_tokens
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

// formatJSON 将 JSON 字节切片格式化为缩进 JSON，限制到 20 KB 并附加省略标记，避免超大 body 撑爆终端。
func formatJSON(raw []byte) string {
	if len(raw) > 20*1024 {
		return string(raw[:20*1024]) + fmt.Sprintf("\n... (body too large, %d bytes total)\n", len(raw))
	}
	var data any
	if json.Unmarshal(raw, &data) != nil {
		return string(raw)
	}
	pretty, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(pretty)
}

// writeSSE 安全写 SSE 事件：返回 true 表示成功，false 表示客户端已断开（ECONNRESET/BROKEN PIPE）
func writeSSE(w http.ResponseWriter, data []byte) bool {
	_, err := w.Write(data)
	if err != nil {
		log.Printf("[passthrough] client disconnected (SSE write error): %v", err)
		return false
	}
	return true
}

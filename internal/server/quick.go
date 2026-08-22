package server

import (
	"bytes"
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
	// OpenAI 兼容端点 prefix（如 Google Gemini 为 "/v1beta/openai"）
	openAIPath string
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
	// 请求体过滤：透传前移除上游不支持的字段（按上游类型自适应）
	requestStripper func(json.RawMessage) json.RawMessage
}

// 心跳格式：Anthropic 标准 ping 事件（必须包含 event: ping 前缀）
// @AI_GUARD: SSE_HEARTBEAT_FORMAT - 心跳格式绝对不可修改
// @CONSTRAINT: 必须用 event: ping\ndata: {"type":"ping"}\n\n 格式
//   - data: {}\n\n → Claude Code 解析为 Anthropic 事件，缺少 type 字段 → 解析失败
//   - data: \n\n → Kimi 等严格客户端对空行做 JSON.parse 报错
//   - : heartbeat\n\n → SSE 注释，Claude Code 不识别为"内容活动"，不重置 HTTP 超时 → 长上游响应时客户端断开
//   - data: {"type":"ping"}\n\n → 缺少 event: 前缀，Claude Code 不识别为 ping 事件，不重置超时 → 仍报 empty response
//   - event: ping\ndata: {"type":"ping"}\n\n → 完整 Anthropic SSE 格式，Claude Code 正确识别并重置超时 ✅
//
// @RELATED: all handlePassthrough* / handleNonStream* handlers that write heartbeat
// @REASON: 历史血泪教训 - 先后尝试过 data: \n\n、data: {}\n\n、: heartbeat\n\n、data: {"type":"ping"}，各有问题
var heartbeatEvent = []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")

// applyRequestStripper 应用上游类型自适应的请求字段过滤（未配置 stripper 时原样返回）
// @AI_GUARD: PASSTHROUGH_REQUEST_FILTER - 所有透传入口点必须调用此方法
func (q *QuickGateway) applyRequestStripper(body json.RawMessage) json.RawMessage {
	if q.requestStripper == nil {
		return body
	}
	return q.requestStripper(body)
}

// applyUpstreamStrip 按上游地址类型应用请求字段过滤（复杂模式 gateway.go 使用，多 provider 按请求判定）
// @AI_GUARD: PASSTHROUGH_REQUEST_FILTER - 与 applyRequestStripper 同等约束
func applyUpstreamStrip(baseURL string, body json.RawMessage) json.RawMessage {
	if fn := newRequestStripper(DetectUpstreamType(baseURL)); fn != nil {
		return fn(body)
	}
	return body
}

// DetectUpstreamType 根据 base URL 域名推断上游类型，用于自动加载请求字段过滤规则
// @REASON: 不同上游对 Anthropic/CC 协议的兼容程度不同，某些字段（guided_grammar/cache_control/effort=xhigh）
//
//	在部分上游会触发 HTTP 400。透传路径不能硬编码特定上游，必须按上游类型自适应。
//	db add 时调用并保存到 DB（upstream_type 列），运行时优先用 DB 值，缺失时回退到域名检测。
func DetectUpstreamType(baseURL string) string {
	host := strings.TrimPrefix(baseURL, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.Split(host, "/")[0]
	host = strings.TrimSuffix(host, "/")
	// 商汤 SenseNova
	if strings.Contains(host, "sensenova") {
		return "sensenova"
	}
	return "default"
}

// newRequestStripper 根据上游类型返回对应的请求体过滤函数
// 新增加场只需在此处添加 case
func newRequestStripper(upstreamType string) func(json.RawMessage) json.RawMessage {
	switch upstreamType {
	case "sensenova":
		// 商汤 SenseNova 不支持:
		// - guided_grammar (无 xgrammar 模块)
		// - cache_control (无缓存功能)
		// - output_config.effort="xhigh" (仅支持 low/medium/high)
		return stripSensenovaRequestFields
	case "default":
		return nil
	default:
		return nil
	}
}

// NewQuickGateway 从 DB 记录创建一个超简易网关
// capabilities: 嗅探到的上游协议列表，如 ["openai", "anthropic", "gemini", "responses"]
// modelsMap: 协议→模型列表映射，如 {"openai":["gpt-4"],"anthropic":["claude-3"]}
// upstreamType: db add 时保存的上游类型（如 "sensenova"），为空时按域名自动检测
// openAIPath: OpenAI 兼容端点 prefix（如 Google Gemini 的 "/v1beta/openai"），为空时回退到 "/v1"
func NewQuickGateway(name, baseURL, apiKey string, capabilities []string, modelsMap map[string][]string, upstreamType string, openAIPath string, timeout int, clientKey string, clientKeyEnabled bool, verboseLevel int) *QuickGateway {
	// 注册 4 个协议翻译器
	registry := translator.NewTranslatorRegistry()
	registry.Register(&chatcompletion.ChatCompletionTranslator{})
	registry.Register(anthropic.NewAnthropicTranslator("2023-06-01"))
	registry.Register(gemini.NewGeminiTranslator())
	registry.Register(responses.NewResponsesTranslator())

	proxyBaseURL := strings.TrimSuffix(strings.TrimSuffix(baseURL, "/"), "/v1")
	if upstreamType == "" {
		upstreamType = DetectUpstreamType(proxyBaseURL)
	}
	stripper := newRequestStripper(upstreamType)
	if stripper != nil {
		log.Printf("[gateway] upstream=%s type=%s, request filter enabled", proxyBaseURL, upstreamType)
	}

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
		proxyBaseURL:       proxyBaseURL,
		proxyKey:           apiKey,
		openAIPath:         openAIPath,
		clientKey:          clientKey,
		clientKeyEnabled:   clientKeyEnabled,
		verboseLevel:       verboseLevel,
		requestStripper:    stripper,
		streamPrefer:       make(map[string]bool),
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
// 策略：归一化后，若该协议在 capabilities 中则使用它（透传）；否则回退到最合适的翻译路径
//   - 出站 chat 协议优先走 openai chat（有 openai 能力则 openai 翻译，无则 gemini 翻译）
//   - 其他协议同理：优先 openai，其次 gemini
func (q *QuickGateway) selectProtocol(ingressProtocol string) string {
	normalized := q.normalizeIngress(ingressProtocol)
	if slices.Contains(q.capabilities, normalized) {
		return normalized
	}
	// 回退：上游不支持该协议 → 翻译路径
	// 出站 chat 优先走 openai chat
	if slices.Contains(q.capabilities, "openai") {
		return "openai"
	}
	if slices.Contains(q.capabilities, "gemini") {
		return "gemini"
	}
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
		p = provider.NewOpenAIClientWithPath(q.proxyName, q.proxyBaseURL, q.timeout, q.openAIPath)
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
	mux.HandleFunc("/v1/responses", q.handleResponses)

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
	//   - 无 stream 字段: 透传非流式 JSON（Anthropic API 标准：stream 默认 false）
	//   - stream:false: 透传非流式
	// @RELATED: handleStreamRequest, handleStreamRequestAsNonStream, handleNonStreamResponse,
	//           handleNonStreamResponseAsSSE, handlePassthroughStreamWithBody,
	//           handlePassthroughNonStream, handlePassthroughNonStreamAsSSE
	// @REASON: 历史血泪教训 - 每修正一种模式可能破坏另一种模式的客户端（Claude Code / Kimi / Codex 行为不同）
	// @REASON: 无 stream 字段改回 JSON - Claude Code /model 验证请求不带 stream，期望 JSON 响应；
	//          包装为 SSE 会导致 "undefined is not an object (evaluating 'K.usage.input_tokens')" 错误
	if normalizedIngress == providerType && realModel != "" {
		if q.verboseLevel >= 2 {
			log.Printf("[route] PASSTHROUGH: model=%s, ingress=%s, provider=%s", realModel, normalizedIngress, providerType)
		}
		ctx := context.WithValue(r.Context(), verboseCtxKey{}, vctx)
		stream := quickDetectStream(body)

		if stream {
			// 流式偏好：按上游地址，首次请求并行竞速 SSE vs 非流式，后续直接用胜出方式
			// 必须用写锁防止并发请求同时看到 tested=false 而重复启动 probe（竞态条件）
			q.streamPreferMu.Lock()
			preferNonStream, tested := q.streamPrefer[q.proxyBaseURL]
			if !tested {
				// 立即标记为已探测（默认非流式），防止并发重复启动 probe
				q.streamPrefer[q.proxyBaseURL] = true
				q.streamPreferMu.Unlock()
				if q.verboseLevel >= 2 {
					log.Printf("[route] stream=true, auto-probe (first request for %s)", q.proxyBaseURL)
				}
				nsStart := time.Now()
				q.handlePassthroughNonStreamAsSSE(p, ctx, w, r, realModel, originalModel, aliasHit, startTime, body)
				callInfo := q.info
				callInfo.Name = realModel
				go q.probeStreamPrefer(p, callInfo, realModel, time.Since(nsStart))
				return
			}
			q.streamPreferMu.Unlock()
			if preferNonStream {
				if q.verboseLevel >= 2 {
					log.Printf("[route] stream=true, prefer non-stream→SSE")
				}
				q.handlePassthroughNonStreamAsSSE(p, ctx, w, r, realModel, originalModel, aliasHit, startTime, body)
			} else {
				// @AI_GUARD: LARGE_BODY_SKIP_STREAM - 大请求跳过原生流式
				// @REASON: SenseNova 等上游对大请求（>100KB）的流式处理会立即失败（502 stream read failed），
				//          probe 用小请求测得 SSE=345ms 选择了流式，但实际大请求流式需 12-18s 才失败。
				//          客户端等不及断开连接（broken pipe），fallback 即使成功也写不回去。
				//          阈值 100KB：超过此大小直接走非流式 SSE 包装，避免长等待。
				const largeBodyThreshold = 100 * 1024 // 100KB
				if len(body) > largeBodyThreshold {
					if q.verboseLevel >= 2 {
						log.Printf("[route] stream=true, large body (%d bytes > %d) → use non-stream→SSE", len(body), largeBodyThreshold)
					}
					q.handlePassthroughNonStreamAsSSE(p, ctx, w, r, realModel, originalModel, aliasHit, startTime, body)
				} else {
					if q.verboseLevel >= 2 {
						log.Printf("[route] stream=true, prefer native stream")
					}
					q.handlePassthroughStream(p, ctx, w, r, realModel, originalModel, aliasHit, startTime)
				}
			}
			// 无 stream 字段或显式 stream:false → 期望 raw JSON（Anthropic API 标准：stream 默认 false）
			// Claude Code /model 验证请求不带 stream 字段，期望 JSON 响应
		} else {
			if q.verboseLevel >= 2 {
				if quickStreamExplicitFalse(body) {
					log.Printf("[route] stream=false, passthrough non-stream (raw JSON)")
				} else {
					log.Printf("[route] no stream field, passthrough non-stream (raw JSON)")
				}
			}
			q.handlePassthroughNonStream(p, ctx, w, r, realModel, originalModel, aliasHit, startTime)
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
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		q.handleResponsesWebSocket(w, r)
		return
	}
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

// stripGuidedGrammarFromRequest 移除透传请求中 sensenova 不支持的字段
// @REASON: Claude Code 客户端在 Anthropic API 请求中携带 sensenova 不兼容的字段：
//   - guided_grammar: sensenova 缺少 xgrammar 模块，返回 HTTP 400
//   - cache_control: sensenova 不支持缓存控制
//   - output_config.effort="xhigh": sensenova 仅支持 low/medium/high
//     透传路径原样转发请求体，必须在发送前处理这些字段。
//
// @AI_GUARD: PASSTHROUGH_REQUEST_FILTER - 所有透传入口点必须调用此函数
func stripSensenovaRequestFields(body json.RawMessage) json.RawMessage {
	if len(body) == 0 {
		return body
	}

	beforeLen := len(body)
	hasGG := strings.Contains(string(body), "guided_grammar")
	log.Printf("[strip] body_len=%d hasGuidedGrammar=%v", beforeLen, hasGG)

	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		log.Printf("[strip] json.Unmarshal FAILED: %v, trying fallback string replace", err)
		s := string(body)
		// 简单策略：找到 "guided_grammar": ... 到下一个顶级字段结束
		for {
			idx := strings.Index(s, `"guided_grammar"`)
			if idx < 0 {
				break
			}
			colonIdx := strings.Index(s[idx:], ":")
			if colonIdx < 0 {
				break
			}
			valStart := idx + colonIdx + 1
			for valStart < len(s) && (s[valStart] == ' ' || s[valStart] == '\t' || s[valStart] == '\n' || s[valStart] == '\r') {
				valStart++
			}
			if valStart >= len(s) {
				break
			}
			open := s[valStart]
			var endIdx int
			switch open {
			case '{':
				endIdx = matchBraces(s, valStart, '{', '}')
			case '[':
				endIdx = matchBraces(s, valStart, '[', ']')
			case '"':
				endIdx = matchString(s, valStart)
			default:
				endIdx = valStart
				for endIdx < len(s) && s[endIdx] != ',' && s[endIdx] != '}' && s[endIdx] != ']' {
					endIdx++
				}
			}
			if endIdx < 0 {
				break
			}
			trailEnd := endIdx + 1
			for trailEnd < len(s) && s[trailEnd] == ' ' {
				trailEnd++
			}
			if trailEnd < len(s) && s[trailEnd] == ',' {
				trailEnd++
			}
			s = s[:idx] + s[trailEnd:]
		}
		log.Printf("[strip] fallback done, body_len=%d→%d", beforeLen, len(s))
		return json.RawMessage(s)
	}
	// 将顶层 system 合并到 messages[0]（role: system）
	// @REASON: SenseNova /v1/messages 端点收到顶层 system 字段会 hang（永不返回），
	//          必须降级为 messages 数组中的 system role 消息。
	if sysVal, hasSystem := raw["system"]; hasSystem {
		msgs, ok := raw["messages"].([]interface{})
		if ok && len(msgs) > 0 {
			hasSysMsg := false
			for _, msg := range msgs {
				if m, ok := msg.(map[string]interface{}); ok {
					if role, ok := m["role"].(string); ok && role == "system" {
						hasSysMsg = true
						break
					}
				}
			}
			if !hasSysMsg {
				contentVal := sysVal
				// Anthropic 标准：content 用 text block 数组；若 sysVal 已是字符串则包裹一层
				if str, isStr := sysVal.(string); isStr {
					contentVal = []interface{}{map[string]interface{}{"type": "text", "text": str}}
				}
				raw["messages"] = append([]interface{}{map[string]interface{}{"role": "system", "content": contentVal}}, msgs...)
				log.Printf("[strip] system merged into messages[0]")
			}
		}
		delete(raw, "system")
	}

	// 递归删除所有层级的不兼容字段 + 修正 output_config.effort
	pruned := stripSensenovaIncompatible(raw)
	out, err := json.Marshal(pruned)
	if err != nil {
		log.Printf("[strip] marshal after prune FAILED: %v", err)
		return body
	}
	log.Printf("[strip] structured strip done, body_len=%d→%d", beforeLen, len(out))
	return json.RawMessage(out)
}

// stripSensenovaIncompatible 递归处理 sensenova 不兼容字段
// 1. 删除所有层级的 guided_grammar
// 2. 删除所有层级的 cache_control
// 3. output_config.effort="xhigh" → "high"
func stripSensenovaIncompatible(v interface{}) interface{} {
	switch m := v.(type) {
	case map[string]interface{}:
		// 删除 guided_grammar 和 cache_control
		delete(m, "guided_grammar")
		delete(m, "cache_control")
		// 修正 output_config.effort
		if oc, ok := m["output_config"].(map[string]interface{}); ok {
			if effort, ok := oc["effort"].(string); ok && effort == "xhigh" {
				oc["effort"] = "high"
				log.Printf("[strip] output_config.effort xhigh→high")
			}
		}
		for k, val := range m {
			m[k] = stripSensenovaIncompatible(val)
		}
		return m
	case []interface{}:
		for i, val := range m {
			m[i] = stripSensenovaIncompatible(val)
		}
		return m
	default:
		return v
	}
}

// removeGuidedGrammarRecursive 递归删除 map 中所有层级的 guided_grammar 键
// @deprecated: 已由 stripSensenovaIncompatible 替代，保留以兼容旧调用点
func removeGuidedGrammarRecursive(v interface{}) interface{} {
	return stripSensenovaIncompatible(v)
}

// matchBraces 返回匹配配对括号后的下一个字符索引（或 -1 表示未匹配）
func matchBraces(s string, start int, open, close rune) int {
	if start >= len(s) {
		return -1
	}
	r := rune(s[start])
	if r != open {
		return -1
	}
	depth := 0
	inStr := false
	escape := false
	for i := start; i < len(s); i++ {
		c := rune(s[i])
		if inStr {
			if escape {
				escape = false
			} else if c == '\\' {
				escape = true
			} else if c == '"' {
				inStr = false
			}
		} else {
			if c == '"' {
				inStr = true
			} else if c == open {
				depth++
			} else if c == close {
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
	}
	return -1
}

// matchString 返回字符串字面量结束的下一个字符索引（或 -1）
func matchString(s string, start int) int {
	if start >= len(s) || rune(s[start]) != '"' {
		return -1
	}
	for i := start + 1; i < len(s); i++ {
		if rune(s[i]) == '\\' {
			i++
		} else if rune(s[i]) == '"' {
			return i + 1
		}
	}
	return -1
}

// stripToolUseDescription 过滤 tool_use.input 中 Claude Code 不接受的多余 description 字段
// @REASON: Fable 5 在 Anthropic tool_use 的 input 中额外添加 description 字段，
//
//	Claude Code 的 Bash tool schema 不接受该字段，导致 "tool call could not be parsed"
//	注意：description 字段在 tool 定义（请求 tools 数组）层面是合法的，
//	只有出现在 tool_use.input 里才是问题。
func stripToolUseDescription(resp json.RawMessage) json.RawMessage {
	if len(resp) == 0 {
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

	filtered := make([]interface{}, 0, len(contentArr))
	for _, item := range contentArr {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		if t, _ := itemMap["type"].(string); t == "tool_use" {
			if input, ok := itemMap["input"].(map[string]interface{}); ok {
				delete(input, "description")
			}
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

// fixNullUsageInResponse 修复透传响应中 usage 为 null 的问题
// Claude Code 解析 K.usage.input_tokens 时若为 null 会报 undefined
// 上游（如 sensenova）可能返回 "usage": null，需要补默认值 {"input_tokens":0,"output_tokens":0}
func fixNullUsageInResponse(resp json.RawMessage) json.RawMessage {
	if len(resp) == 0 {
		return resp
	}
	// 快速检测：如果包含 "usage":null 或不包含 "usage" 键，需要修复
	hasNullUsage := bytes.Contains(resp, []byte(`"usage":null`)) || bytes.Contains(resp, []byte(`"usage": null`))
	hasUsageKey := bytes.Contains(resp, []byte(`"usage":`))
	if !hasNullUsage && hasUsageKey {
		return resp
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(resp, &raw); err != nil {
		return resp
	}

	// 检测响应格式：Anthropic（有 content 数组）/ ChatCompletion（有 choices 数组）
	isAnthropic := false
	isCC := false
	if _, ok := raw["content"].([]interface{}); ok {
		isAnthropic = true
	}
	if _, ok := raw["choices"].([]interface{}); ok {
		isCC = true
	}

	needsFix := false
	usageVal, usageExists := raw["usage"]
	if !usageExists {
		needsFix = true
	} else if usageVal == nil {
		needsFix = true
	}

	if !needsFix {
		return resp
	}

	if isAnthropic {
		raw["usage"] = map[string]interface{}{
			"input_tokens":  0,
			"output_tokens": 0,
		}
	} else if isCC {
		raw["usage"] = map[string]interface{}{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		}
	} else {
		// 未知格式：同时写入两种字段，安全兜底
		raw["usage"] = map[string]interface{}{
			"input_tokens":      0,
			"output_tokens":     0,
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		}
	}

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
// Anthropic API 标准：stream 默认 false。无 stream 字段或 stream:false → JSON；stream:true → SSE。
// Claude Code /model 验证请求不带 stream 字段，期望 JSON 响应。
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

	// 上游类型自适应字段过滤：必须在 alias 字符串替换之前处理
	body = q.applyRequestStripper(body)

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
		// @AI_GUARD: STREAM_FALLBACK_NONSTREAM - 流式失败降级为非流式 SSE 包装
		// @CONSTRAINT: SSE 头已发送（WriteHeader(200)），不能改回 JSON，必须用 writeNonStreamAsSSE 包装
		// @REASON: 上游（如 SenseNova）对大请求（>100KB）的流式处理可能立即失败（context canceled），
		//          但非流式 Call 能正常返回。降级确保客户端仍收到完整 SSE 响应。
		log.Printf("[passthrough] upstream stream error: %s=%s url=%s body_len=%d err=%v — fallback to non-stream→SSE",
			aliasModel, realModel, q.proxyBaseURL, len(body), err)
		if q.verboseLevel >= 2 {
			log.Printf("[passthrough] stream failed, fallback to non-stream→SSE")
		}
		// @AI_GUARD: FALLBACK_DETACHED_CONTEXT - fallback 必须用独立 Background ctx，不能用请求 ctx
		nsBody := quickRemoveStreamFlag(body)
		// 上游类型自适应字段过滤（fallback 路径再过滤一次确保生效）
		nsBody = q.applyRequestStripper(nsBody)
		hbCtx := ctx
		if hbCtx.Err() != nil {
			hbCtx = context.Background()
		}
		callDone2, callFinished2 := StartSSEHeartbeat(w, flusher, hbCtx, q.verboseLevel)
		fbCtx, fbCancel := context.WithTimeout(context.Background(), time.Duration(q.timeout)*time.Second)
		respBody, _, err2 := p.Call(fbCtx, nsBody, callInfo)
		fbCancel()
		close(callDone2)
		<-callFinished2
		if err2 != nil {
			log.Printf("[passthrough] fallback non-stream also failed: %s=%s ctx_err=%v err=%v", aliasModel, realModel, ctx.Err(), err2)
			sendSSEErrorFromUpstream(w, flusher, fmt.Errorf("stream error: %w; fallback non-stream error: %w", err, err2))
			return
		}
		// 过滤 thinking + description + 修复 usage
		respBody = stripThinkingContentBlocks(respBody)
		respBody = stripToolUseDescription(respBody)
		respBody = fixNullUsageInResponse(respBody)
		effectiveModel := realModel
		if aliasHit && aliasModel != "" {
			effectiveModel = aliasModel
		}
		writeNonStreamAsSSE(w, flusher, respBody, effectiveModel)
		vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
		vctx.upstreamReq = nsBody
		vctx.upstreamResp = respBody
		q.logRequest(vctx, startTime, http.StatusOK, extractUsage(respBody), nil)
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
					// @AI_GUARD: STREAM_FALLBACK_NONSTREAM - 流式 channel 内部错误降级
					// @REASON: CallStream 成功但 channel 首事件为 _type=error（SenseNova 大请求），
					//          必须降级为非流式 Call + SSE 包装，而非直接报 SSE error
					errData, _ := meta["data"].(string)
					log.Printf("[passthrough] upstream stream error (in-channel): %s=%s url=%s body_len=%d err=%s — fallback to non-stream→SSE",
						aliasModel, realModel, q.proxyBaseURL, len(body), errData)
					if q.verboseLevel >= 2 {
						log.Printf("[passthrough] in-channel stream failed, fallback to non-stream→SSE")
					}
					heartbeat.Stop()
					// @AI_GUARD: FALLBACK_DETACHED_CONTEXT - 与请求 ctx 解绑，避免链式取消
					nsBody := quickRemoveStreamFlag(body)
					hbCtx := ctx
					if hbCtx.Err() != nil {
						hbCtx = context.Background()
					}
					callDone2, callFinished2 := StartSSEHeartbeat(w, flusher, hbCtx, q.verboseLevel)
					fbCtx, fbCancel := context.WithTimeout(context.Background(), time.Duration(q.timeout)*time.Second)
					respBody, _, err2 := p.Call(fbCtx, nsBody, callInfo)
					fbCancel()
					close(callDone2)
					<-callFinished2
					if err2 != nil {
						log.Printf("[passthrough] fallback non-stream also failed: %s=%s ctx_err=%v err=%v", aliasModel, realModel, ctx.Err(), err2)
						sendSSEErrorBody(w, flusher, "api_error", fmt.Sprintf("stream error: %s; fallback non-stream error: %v", errData, err2))
						return
					}
					respBody = stripThinkingContentBlocks(respBody)
					respBody = stripToolUseDescription(respBody)
					respBody = fixNullUsageInResponse(respBody)
					effectiveModel := realModel
					if aliasHit && aliasModel != "" {
						effectiveModel = aliasModel
					}
					writeNonStreamAsSSE(w, flusher, respBody, effectiveModel)
					vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
					vctx.upstreamReq = nsBody
					vctx.upstreamResp = respBody
					q.logRequest(vctx, startTime, http.StatusOK, extractUsage(respBody), nil)
					return
				} else {
					// 过滤非标准 thinking 内容块：SenseNova 的 DeepSeek 模型返回
					// 缺少 signature 字段的非标准 thinking 块，Claude Code 无法解析。
					// 标准 Anthropic thinking 块（含 signature）会被保留。
					eventType, _ := meta["type"].(string)
					modifiedLine := false
					switch eventType {
					case "content_block_start":
						var ct string
						if cb, ok := meta["content_block"].(map[string]any); ok {
							if cbtype, ok := cb["type"].(string); ok {
								ct = cbtype
							}
							if ct == "thinking" {
								if _, hasSig := cb["signature"]; hasSig {
									// 标准 thinking 块，保留
								} else {
									inThinkingBlock = true
									continue
								}
							}
							if ct == "tool_use" {
								if input, ok := cb["input"].(map[string]interface{}); ok {
									delete(input, "description")
									modifiedLine = true
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
					if modifiedLine {
						modifiedJSON, _ := json.Marshal(meta)
						line = append([]byte("data: "), modifiedJSON...)
					}
					usage := extractUsage(trimSSEDataPrefix(line))
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
	// 上游类型自适应字段过滤：必须在 alias 字符串替换之前处理
	body = q.applyRequestStripper(body)

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

	// 透传下游响应头（过滤连接管理 header）
	// @AI_GUARD: PASSTHROUGH_RESPONSE_HEADERS - 必须过滤 Connection/Keep-Alive 等连接管理 header
	// @REASON: 上游（如 SenseNova）返回的 Connection: close / Keep-Alive header 会干扰
	//          Claude Code 的连接复用，导致 "socket connection was closed unexpectedly"
	for k, v := range headers {
		if isConnectionManagementHeader(k) {
			continue
		}
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

	// 过滤 tool_use.input 中 Claude Code 不接受的多余 description 字段
	outResp = stripToolUseDescription(outResp)

	// 修复 usage 为 null 的问题：Claude Code 解析 K.usage.input_tokens 时若为 null 会报 undefined
	// 上游（如 sensenova）可能返回 usage: null，需要补默认值
	outResp = fixNullUsageInResponse(outResp)

	if _, err := w.Write(outResp); err != nil {
		log.Printf("[passthrough] write error: %s=%s url=%s err=%v",
			aliasModel, realModel, q.proxyBaseURL, err)
		return
	}

	vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
	vctx.upstreamReq = body
	vctx.upstreamResp = resp
	vctx.outgoingBody = outResp
	q.logRequest(vctx, startTime, http.StatusOK, extractUsage(outResp), nil)
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
	// 上游类型自适应字段过滤：必须在 alias 字符串替换之前处理
	body = q.applyRequestStripper(body)

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
		// @AI_GUARD: STREAM_FALLBACK_NONSTREAM - 流式失败降级为非流式 SSE 包装
		// @CONSTRAINT: SSE 头已发送（WriteHeader(200)），不能改回 JSON，必须用 writeNonStreamAsSSE 包装
		// @REASON: 上游（如 SenseNova）对大请求（>100KB）的流式处理可能立即失败（context canceled），
		//          但非流式 Call 能正常返回。降级确保客户端仍收到完整 SSE 响应。
		log.Printf("[passthrough] upstream stream error: %s=%s url=%s body_len=%d err=%v — fallback to non-stream→SSE",
			aliasModel, realModel, q.proxyBaseURL, len(body), err)
		if q.verboseLevel >= 2 {
			log.Printf("[passthrough] stream failed, fallback to non-stream→SSE")
		}
		// @AI_GUARD: FALLBACK_DETACHED_CONTEXT - fallback 必须用独立 Background ctx，不能用请求 ctx
		// @REASON: 上游流式失败常伴随 callCtx 取消（"stream read failed: context canceled"），
		//          callCtx 父链包含请求 ctx，若父链被取消 fallback Call 会立即失败。
		//          改用 Background+timeout 做独立的 fallback 请求，与请求 ctx 取消解绑。
		nsBody := quickRemoveStreamFlag(body)
		hbCtx := ctx
		if hbCtx.Err() != nil {
			hbCtx = context.Background()
		}
		callDone2, callFinished2 := StartSSEHeartbeat(w, flusher, hbCtx, q.verboseLevel)
		fbCtx, fbCancel := context.WithTimeout(context.Background(), time.Duration(q.timeout)*time.Second)
		respBody, _, err2 := p.Call(fbCtx, nsBody, callInfo)
		fbCancel()
		close(callDone2)
		<-callFinished2
		if err2 != nil {
			log.Printf("[passthrough] fallback non-stream also failed: %s=%s ctx_err=%v err=%v", aliasModel, realModel, ctx.Err(), err2)
			sendSSEErrorFromUpstream(w, flusher, fmt.Errorf("stream error: %w; fallback non-stream error: %w", err, err2))
			return
		}
		// 过滤 thinking + description + 修复 usage
		respBody = stripThinkingContentBlocks(respBody)
		respBody = stripToolUseDescription(respBody)
		respBody = fixNullUsageInResponse(respBody)
		effectiveModel := realModel
		if aliasHit && aliasModel != "" {
			effectiveModel = aliasModel
		}
		writeNonStreamAsSSE(w, flusher, respBody, effectiveModel)
		vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
		vctx.upstreamReq = nsBody
		vctx.upstreamResp = respBody
		q.logRequest(vctx, startTime, http.StatusOK, extractUsage(respBody), nil)
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
					// @AI_GUARD: STREAM_FALLBACK_NONSTREAM - 流式 channel 内部错误降级
					// @CONSTRAINT: CallStream 成功返回 channel，但 channel 中传了错误事件（_type=="error"）
					//   必须同样降级为非流式 Call + SSE 包装，而不是直接报 SSE error。
					// @REASON: SenseNova 对大请求流式处理，CallStream 立即成功（无 error），
					//   但 channel 首条事件就是 {"_type":"error","_status":502,"data":"stream read failed: context canceled"}。
					//   此时心跳 ticker 已启动，需先停止再做非流式 fallback。
					status, _ := meta["_status"].(float64)
					if status == 0 {
						status = 502
					}
					errData, _ := meta["data"].(string)
					log.Printf("[passthrough] upstream stream error (in-channel): %s=%s url=%s body_len=%d status=%v err=%s — fallback to non-stream→SSE",
						aliasModel, realModel, q.proxyBaseURL, len(body), status, errData)
					if q.verboseLevel >= 2 {
						log.Printf("[passthrough] in-channel stream failed, fallback to non-stream→SSE")
					}
					// 停止当前的 stream 心跳（ticker 在 for 循环外层 defer）
					heartbeat.Stop()
					// @AI_GUARD: FALLBACK_DETACHED_CONTEXT - 与请求 ctx 解绑，避免链式取消
					nsBody := quickRemoveStreamFlag(body)
					nsBody = q.applyRequestStripper(nsBody)
					hbCtx := ctx
					if hbCtx.Err() != nil {
						hbCtx = context.Background()
					}
					callDone2, callFinished2 := StartSSEHeartbeat(w, flusher, hbCtx, q.verboseLevel)
					fbCtx, fbCancel := context.WithTimeout(context.Background(), time.Duration(q.timeout)*time.Second)
					respBody, _, err2 := p.Call(fbCtx, nsBody, callInfo)
					fbCancel()
					close(callDone2)
					<-callFinished2
					if err2 != nil {
						log.Printf("[passthrough] fallback non-stream also failed: %s=%s ctx_err=%v err=%v", aliasModel, realModel, ctx.Err(), err2)
						// Fallback 也失败，再发送 SSE error
						msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
						echoModel := realModel
						if aliasHit && aliasModel != "" {
							echoModel = aliasModel
						}
						w.Write([]byte(`event: message_start` + "\n" +
							fmt.Sprintf(`data: {"type":"message_start","message":{"id":"%s","type":"message","role":"assistant","content":[],"model":"%s","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`, msgID, echoModel) + "\n\n"))
						sendSSEErrorFromUpstream(w, flusher, fmt.Errorf("stream error: %s; fallback non-stream error: %w", errData, err2))
						return
					}
					// 过滤 thinking + description + 修复 usage
					respBody = stripThinkingContentBlocks(respBody)
					respBody = stripToolUseDescription(respBody)
					respBody = fixNullUsageInResponse(respBody)
					effectiveModel := realModel
					if aliasHit && aliasModel != "" {
						effectiveModel = aliasModel
					}
					writeNonStreamAsSSE(w, flusher, respBody, effectiveModel)
					vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
					vctx.upstreamReq = nsBody
					vctx.upstreamResp = respBody
					q.logRequest(vctx, startTime, http.StatusOK, extractUsage(respBody), nil)
					return
				}
			}
			// 累积 usage（用于 -v 日志）
			usage := extractUsage(trimSSEDataPrefix(line))
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
				// 先发 message_start（Claude Code 解析器要求 SSE 流以 message_start 开头）
				msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
				w.Write([]byte(`event: message_start` + "\n" +
					fmt.Sprintf(`data: {"type":"message_start","message":{"id":"%s","type":"message","role":"assistant","content":[],"model":"","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`, msgID) + "\n\n"))
				// 直接透传上游错误体
				errJSON, _ := json.Marshal(upstreamErr)
				w.Write([]byte("event: error\ndata: "))
				w.Write(errJSON)
				w.Write([]byte("\n\n"))
				// 发送 message_stop 终止 SSE 流（带 event: 前缀）
				w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
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
// @CONSTRAINT: 错误事件格式：message_start → event: error → event: message_stop
//   - 必须先发 message_start（Claude Code 要求 SSE 事件序列以 message_start 开头，否则报 "empty or malformed response"）
//   - 必须发送 message_stop 终止 SSE 流（否则客户端等待超时）
//   - message_stop 必须带 event: 前缀（Anthropic 标准 SSE 事件格式）
func sendSSEErrorBody(w http.ResponseWriter, flusher http.Flusher, errorType, errorMessage string) {
	// 先发 message_start（Claude Code 解析器要求 SSE 流以 message_start 开头）
	// id 不能为空，否则 Claude Code 报 "empty or malformed response"
	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	w.Write([]byte(`event: message_start` + "\n" +
		fmt.Sprintf(`data: {"type":"message_start","message":{"id":"%s","type":"message","role":"assistant","content":[],"model":"","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`, msgID) + "\n\n"))

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
	// 发送 message_stop 终止 SSE 流（带 event: 前缀）
	w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
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
func writeNonStreamAsSSE(w http.ResponseWriter, flusher http.Flusher, respBody []byte, effectiveModel string) *schema.InternalUsage {
	usage := extractUsage(respBody)

	var respMap map[string]interface{}
	if err := json.Unmarshal(respBody, &respMap); err != nil {
		var compactBuf bytes.Buffer
		if err := json.Compact(&compactBuf, respBody); err != nil {
			compactBuf.Reset()
			compactBuf.Write(respBody)
		}
		// 原子写入（避免心跳 goroutine 在 Write 间隙插入打断 SSE 事件）
		buf := make([]byte, 0, len("data: ")+compactBuf.Len()+len("\n\n"))
		buf = append(buf, []byte("data: ")...)
		buf = append(buf, compactBuf.Bytes()...)
		buf = append(buf, '\n', '\n')
		w.Write(buf)
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
			// - usage: {input_tokens, output_tokens:0}（必须是对象，不能为 null；流式开始 output_tokens 为 0，最终值在 message_delta）
			inputTokens := 0
			if usage != nil {
				inputTokens = usage.PromptTokens
			}
			messageStart := fmt.Sprintf(
				`{"type":"message_start","message":{"id":"%s","type":"message","role":"assistant","content":[],"model":"%s","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":0}}}`,
				msgID, effectiveModel, inputTokens)
			if !writeSSE(w, []byte("event: message_start\ndata: "+messageStart+"\n\n")) {
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

				// content_block_start（按 Anthropic 标准补全各块类型必填字段）
				// @AI_GUARD: NONSTREAM_AS_SSE_TOOL_USE - tool_use 块的 id/name 必填，否则
				// Claude Code 报 "The model's tool call could not be parsed"
				blockStartMap := map[string]interface{}{"type": blockType}
				switch blockType {
				case "text":
					blockStartMap["text"] = ""
					blockStartMap["citations"] = []interface{}{}
				case "tool_use":
					toolID, _ := blockMap["id"].(string)
					if toolID == "" {
						toolID = fmt.Sprintf("toolu_%d", time.Now().UnixNano())
					}
					blockStartMap["id"] = toolID
					if name, ok := blockMap["name"].(string); ok {
						blockStartMap["name"] = name
					}
					blockStartMap["input"] = map[string]interface{}{}
				case "thinking":
					blockStartMap["thinking"] = ""
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
				if !writeSSE(w, []byte("event: content_block_start\ndata: "+string(blockStartJSON)+"\n\n")) {
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
					case "tool_use":
						// tool_use 的 input 通过 input_json_delta 以 JSON 字符串发送
						inputRaw := blockMap["input"]
						if inputRaw == nil {
							inputRaw = map[string]interface{}{}
						}
						inputJSON, ierr := json.Marshal(inputRaw)
						if ierr != nil {
							return nil, nil
						}
						return json.Marshal(map[string]interface{}{
							"type":  "content_block_delta",
							"index": idx,
							"delta": map[string]interface{}{
								"type":         "input_json_delta",
								"partial_json": string(inputJSON),
							},
						})
					default:
						return nil, nil
					}
				}()
				if err != nil || len(deltaJSON) == 0 {
					continue
				}
				if !writeSSE(w, []byte("event: content_block_delta\ndata: "+string(deltaJSON)+"\n\n")) {
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
				if !writeSSE(w, []byte("event: content_block_stop\ndata: "+string(blockStopJSON)+"\n\n")) {
					return usage
				}
			}

			// message_delta: 按 Anthropic 标准，usage 仅含 output_tokens（input_tokens 已在 message_start 给出）
			// delta 必须包含 stop_sequence:null；usage 始终存在（即使为 0），确保客户端能解析 K.usage
			outputTokens := 0
			if usage != nil {
				outputTokens = usage.CompletionTokens
			}
			messageDelta := fmt.Sprintf(
				`{"type":"message_delta","delta":{"stop_reason":"%s","stop_sequence":null},"usage":{"output_tokens":%d}}`,
				stopReason, outputTokens)
			if !writeSSE(w, []byte("event: message_delta\ndata: "+messageDelta+"\n\n")) {
				return usage
			}
			// message_stop
			if !writeSSE(w, []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")) {
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
//
// @AI_GUARD: PASSTHROUGH_NONSTREAM_AS_SSE - 透传非流→SSE 包装，必须同步 gateway.go
// @CONSTRAINT: 心跳必须在 Call 之前启动，防止上游慢响应（> 30s）时客户端超时断开
//   - 先设 SSE 头 + WriteHeader(200) + Flush()，确保心跳可写
//   - 启动心跳 goroutine，在 Call 阻塞期间持续发送心跳
//   - Call 返回后先停心跳 (close(callDone) → <-callFinished) 再写 SSE 响应
//   - 错误路径：SSE 头已设置，必须发 SSE error 事件
//
// @RELATED: StartSSEHeartbeat, writeNonStreamAsSSE, handleNonStreamResponseAsSSE (翻译路径对应)
// @REASON: 历史血泪教训 - 心跳在 Call 之后启动，71s 上游响应期间 0 beats，
//
//	客户端超时断开报 "API returned an empty or malformed response (HTTP 200)"
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

	// 上游类型自适应字段过滤：必须在 alias 字符串替换之前处理
	nsBody = q.applyRequestStripper(nsBody)

	if aliasHit && aliasModel != "" {
		nsBody = quickReplaceModelInBody(nsBody, aliasModel, realModel)
	}

	// 先设 SSE 头 + WriteHeader(200) + Flush()，确保心跳可写
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
	flusher.Flush()

	// 在 Call 阻塞等待上游响应期间发送 SSE 心跳
	callDone, callFinished := StartSSEHeartbeat(w, flusher, r.Context(), q.verboseLevel)

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(q.timeout)*time.Second)
	upstreamStart := time.Now()
	respBody, _, err := p.Call(callCtx, nsBody, callInfo)
	cancel()
	if q.verboseLevel >= 2 {
		log.Printf("[upstream] Call %s → %v", realModel, time.Since(upstreamStart))
	}
	close(callDone) // 停止心跳
	<-callFinished  // 等待心跳 goroutine 退出，防止并发写

	if err != nil {
		log.Printf("[passthrough] nonstream-as-sse error: %s=%s url=%s err=%v",
			aliasModel, realModel, q.proxyBaseURL, err)
		if q.verboseLevel >= 2 {
			log.Printf("[sse-error] passthrough-nonstream-as-sse → SSE error event (was JSON)")
		}
		sendSSEErrorFromUpstream(w, flusher, err)
		flusher.Flush()
		return
	}

	// 回显客户端模型名（嵌入到 SSE 事件的 model 字段）
	effectiveModel := realModel
	if aliasHit && aliasModel != "" {
		effectiveModel = aliasModel
	}

	// 过滤 thinking 内容块（同 handlePassthroughNonStream）
	respBody = stripThinkingContentBlocks(respBody)

	// 过滤 tool_use.input 中 Claude Code 不接受的多余 description 字段
	respBody = stripToolUseDescription(respBody)

	// 修复 usage 为 null 的问题：Claude Code 解析 K.usage.input_tokens 时若为 null 会报 undefined
	respBody = fixNullUsageInResponse(respBody)

	usage := writeNonStreamAsSSE(w, flusher, respBody, effectiveModel)

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
// @AI_GUARD: NONSTREAM_RESPONSE_AS_SSE - 翻译路径非流式→SSE 包装
// @CONSTRAINT: 心跳必须在 Call 之前启动，防止上游慢响应时客户端超时断开
//   - 先设 SSE 头 + WriteHeader(200) + Flush()，确保心跳可写
//   - 启动心跳 goroutine，在 Call 阻塞期间持续发送心跳
//   - Call 返回后先停心跳 (close(callDone) → <-callFinished) 再处理响应
//   - 错误路径：SSE 头已设置，必须发 SSE error 事件
//
// @RELATED: StartSSEHeartbeat, handlePassthroughNonStreamAsSSE (透传路径对应)
// @REASON: 历史血泪教训 - 心跳在 Call 之后启动，上游慢响应期间 0 beats，客户端超时断开
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

	// 先设 SSE 头 + WriteHeader(200) + Flush()，确保心跳可写
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
	flusher.Flush()

	// 在 Call 阻塞等待上游响应期间发送 SSE 心跳
	callDone, callFinished := StartSSEHeartbeat(w, flusher, ctx, q.verboseLevel)

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(q.timeout)*time.Second)
	upstreamStart := time.Now()
	resp, headers, err := p.Call(callCtx, downstreamReq, q.info)
	cancel()
	if q.verboseLevel >= 2 {
		log.Printf("[upstream] Call %s → %v", internalReq.Model, time.Since(upstreamStart))
	}
	close(callDone) // 停止心跳
	<-callFinished  // 等待心跳 goroutine 退出，防止并发写

	if err != nil {
		if q.verboseLevel >= 2 {
			log.Printf("[sse-error] translation-nonstream-as-sse → SSE error event (was JSON)")
		}
		sendSSEErrorFromUpstream(w, flusher, err)
		flusher.Flush()
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
			// 心跳已停止（Call 返回后 close(callDone)），此处直接写 SSE 错误
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
			// 心跳已停止，直接写 SSE 错误
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
		// 心跳已停止，直接写 SSE 错误
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
	usage := writeNonStreamAsSSE(w, flusher, outgoingResp, effectiveModel)

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
// @AI_GUARD: HANDLE_STREAM_REQUEST - 翻译路径流式处理，必须同步 gateway.go
// @CONSTRAINT: 两阶段心跳保护：
//   - 阶段 1: CallStream 期间的心跳（保护 HTTP 连接建立）
//   - 阶段 2: 流处理期间的心跳（保护等待上游首个数据到达）
//     每个阶段必须独立使用 callDone/callFinished 防止并发写
//
// @RELATED: StartSSEHeartbeat, TranslateStream
// @REASON: 历史血泪教训 - 只有阶段 1 心跳，CallStream 返回后 channel 等待数据期间无心跳，
//
//	客户端超时断开。71s 上游响应就是因为这个问题。
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

	// ⚠️ Responses 协议完全禁用 SSE 心跳：Codex 客户端严格校验 event 类型，
	//   event: ping 不属于 OpenAI Responses API 事件集，插入后会导致其解析器
	//   状态机异常，报 "stream closed before response.completed"。
	//   Responses API 通过流动的 SSE 数据事件保持连接，无需额外心跳。
	isResponsesIngress := internalReq != nil && internalReq.Protocol == "responses"

	// ── 阶段 1: CallStream 期间的心跳 ──
	var callDone1 chan struct{}
	var callFinished1 chan struct{}
	if isResponsesIngress {
		callDone1, callFinished1 = newDummyHeartbeat()
	} else {
		callDone1, callFinished1 = StartSSEHeartbeat(w, flusher, ctx, q.verboseLevel)
	}

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(q.timeout)*time.Second)
	defer cancel()

	upstreamStart := time.Now()
	lines, _, err := p.CallStream(callCtx, downstreamReq, q.info)
	if q.verboseLevel >= 2 {
		log.Printf("[upstream] CallStream %s → %v", internalReq.Model, time.Since(upstreamStart))
	}
	close(callDone1) // 停止阶段 1 心跳
	<-callFinished1  // 等待心跳 goroutine 退出

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

	// ── 阶段 2: 流处理期间的心跳（保护等待上游数据到达的空窗期） ──
	// 使用 MutexSSEWriter 防止心跳 goroutine 与 TranslateStream 回调并发写 w
	mw := NewMutexSSEWriter(w, flusher)
	var callDone2 chan struct{}
	var callFinished2 chan struct{}
	if isResponsesIngress {
		callDone2, callFinished2 = newDummyHeartbeat()
	} else {
		callDone2, callFinished2 = StartSSEHeartbeat(mw, mw, ctx, q.verboseLevel)
	}

	// 构建内部流式事件 channel
	events := make(chan schema.InternalStreamEvent, 16)
	var accumulatedUsage *schema.InternalUsage
	go func() {
		defer close(events)
		ccStartSent := false // OpenAI 兼容路径是否已发送 start 事件
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
					// 首个事件就是错误时，先发 start 让 Responses 翻译器生成 response.created
					if !ccStartSent {
						ccStartSent = true
						events <- schema.InternalStreamEvent{
							Type: "start",
							Data: &schema.InternalStreamChunk{
								Model: internalReq.Model,
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
				// 首个有效 delta 事件前先发 start，Responses 入站翻译器据此生成 response.created
				// Codex 期望完整事件序列：response.created → response.output_delta → response.completed
				if !ccStartSent {
					ccStartSent = true
					modelName := ccChunk.Model
					if aliasModel != "" {
						modelName = aliasModel
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
				text := choice.Delta.Content
				// ⚠️ Responses 协议：绝不能把上游 reasoning/thinking 当 output_text 正文输出
				//   Responses 有独立的 response.output_text.delta / thinking 事件体系，
				//   混发 thinking → Codex 解析器把思考当正文造成后续逻辑错乱
				//   （如 thinking 开头的"思考"被当作正式回答的一部分）。
				//   仅在非 Responses 入站时（CC/Anthropic 等）允许 Reasoning 回退：
				//   Anthropic 入站的 TranslateStream 会单独合成 thinking 块。
				if text == "" && !isResponsesIngress {
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
	// 阶段 2 心跳保护整个流处理过程（等待上游数据到达 + TranslateStream 写入）
	ingressTranslator.TranslateStream(ctx, events, func(eventData []byte, isDone bool) {
		mw.Write(eventData)
		mw.Flush()
	})

	// 流处理完成，停止阶段 2 心跳
	close(callDone2)
	<-callFinished2

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

// isConnectionManagementHeader 判断是否为连接管理 header
// 这些 header 由 HTTP 层管理，透传会干扰下游客户端（如 Claude Code）的连接复用
// 返回 true 表示应该被过滤（不向下游透传）
func isConnectionManagementHeader(header string) bool {
	switch strings.ToLower(header) {
	case "connection", "keep-alive", "transfer-encoding", "upgrade",
		"proxy-connection", "trailer", "content-length":
		return true
	default:
		return false
	}
}

// handleModels 透传上游 /v1/models，实时获取模型列表
// @AI_GUARD: MODELS_UPSTREAM_TIMEOUT - 必须使用带超时的专用客户端
// @REASON: Claude Code 在 /model 命令切换模型时会调用 GET /v1/models 验证模型；
//
//	若上游（如 SenseNova）响应慢，http.DefaultClient（无超时）导致代理无限等待，
//	客户端超时断开 → "socket connection was closed unexpectedly"
//
// @REASON: 透传上游 Connection header 会干扰 Claude Code 连接复用；
//
//	必须过滤 Connection / Keep-Alive / Transfer-Encoding / Upgrade 等连接管理 header
func (q *QuickGateway) handleModels(w http.ResponseWriter, r *http.Request) {
	modelsURL := q.proxyBaseURL + "/v1/models"
	req, err := http.NewRequestWithContext(r.Context(), "GET", modelsURL, nil)
	if err != nil {
		q.sendError(w, http.StatusInternalServerError, "proxy_error", err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+q.proxyKey)
	req.Header.Set("Accept", "application/json")

	// 使用带超时的专用客户端，避免上游慢响应导致代理无限等待
	modelsClient := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     30 * time.Second,
		},
	}
	resp, err := modelsClient.Do(req)
	if err != nil {
		q.sendError(w, http.StatusBadGateway, "upstream_unreachable", err.Error())
		return
	}
	defer resp.Body.Close()

	// 透传上游 Content-Type 和状态码相关的响应头，但过滤连接管理 header
	// 避免上游 Connection: close / Keep-Alive 等干扰下游 Claude Code 连接复用
	for k, v := range resp.Header {
		if isConnectionManagementHeader(k) {
			continue
		}
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}

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
func extractUsage(resp []byte) *schema.InternalUsage {
	var m map[string]any
	if err := json.Unmarshal(resp, &m); err != nil {
		return nil
	}
	usageMap, ok := m["usage"].(map[string]any)
	if !ok {
		return nil
	}

	foundAny := false
	usage := &schema.InternalUsage{}
	if v, ok := usageMap["prompt_tokens"].(float64); ok {
		usage.PromptTokens = int(v)
		foundAny = true
	}
	if v, ok := usageMap["completion_tokens"].(float64); ok {
		usage.CompletionTokens = int(v)
		foundAny = true
	}
	// Anthropic/Responses 用 input_tokens / output_tokens
	if v, ok := usageMap["input_tokens"].(float64); ok {
		usage.PromptTokens = int(v)
		foundAny = true
	}
	if v, ok := usageMap["output_tokens"].(float64); ok {
		usage.CompletionTokens = int(v)
		foundAny = true
	}
	if v, ok := usageMap["total_tokens"].(float64); ok {
		usage.TotalTokens = int(v)
		foundAny = true
	}
	if v, ok := usageMap["cache_creation_input_tokens"].(float64); ok {
		usage.CacheCreationTokens = int(v)
		foundAny = true
	}
	if v, ok := usageMap["cache_read_input_tokens"].(float64); ok {
		usage.CacheReadTokens = int(v)
		foundAny = true
	}
	// 只有 usage 字段存在但没有任何可识别的 token 数字时，才返回 nil
	// 全 0 是合法值（如空响应），不能返回 nil
	if !foundAny {
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

// ═══════════════════════════════════════════════════════════════════════════════
//  WebSocket 支持 — OpenAI Codex 通过 WebSocket 连接 /v1/responses
// ═══════════════════════════════════════════════════════════════════════════════

const (
	qwsGUID         = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	qwsOpText  byte = 0x1 // text frame
	qwsOpClose byte = 0x8 // close frame
	qwsFin          = 0x80
	qwsMask         = 0x80
)

// @AI_GUARD: QUICK_WEBSOCKET_SUPPORT - 快速模式 Codex 支持（需同步 gateway.go handleResponsesWebSocket）
// @CONSTRAINT: handleResponses 入口必须用 mux.HandleFunc 注册（不能用 mux.Post），否则 chi 在方法过滤阶段就返回 405，handler 不会被调用
// @REASON: 历史血泪教训 - 之前用 mux.Post 注册，Codex 的 ws:// 连接（GET + Upgrade）被 chi 直接 405，handleResponses 完全收不到请求
// @RELATED: gateway.go handleResponsesWebSocket, gateway.go Routes(), quick.go Routes()

func quickComputeAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key + qwsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func quickReadWSFrame(conn net.Conn) ([]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	masked := header[1]&qwsMask != 0
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

func quickWriteWSFrame(conn net.Conn, payload []byte) error {
	length := len(payload)
	var frame []byte
	switch {
	case length < 126:
		frame = make([]byte, 2+length)
		frame[0] = qwsFin | qwsOpText
		frame[1] = byte(length)
		copy(frame[2:], payload)
	case length < 65536:
		frame = make([]byte, 4+length)
		frame[0] = qwsFin | qwsOpText
		frame[1] = 126
		binary.BigEndian.PutUint16(frame[2:4], uint16(length))
		copy(frame[4:], payload)
	default:
		frame = make([]byte, 10+length)
		frame[0] = qwsFin | qwsOpText
		frame[1] = 127
		binary.BigEndian.PutUint64(frame[2:10], uint64(length))
		copy(frame[10:], payload)
	}
	_, err := conn.Write(frame)
	return err
}

type qwsResponseWriter struct {
	conn        net.Conn
	header      http.Header
	wroteHeader bool
}

func (w *qwsResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *qwsResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if len(b) == 0 {
		return 0, nil
	}
	if err := quickWriteWSFrame(w.conn, b); err != nil {
		return 0, err
	}
	// @CODEX-DEBUG v0.2.98：每帧写出记录（生产可见，前 120 字节用于定位生命周期事件）
	preview := strings.ReplaceAll(strings.TrimRight(string(b[:min(len(b), 120)]), "\n"), "\r", "")
	log.Printf("[CODEX-DEBUG] WS frame → client: bytes=%d preview=%q", len(b), preview)
	return len(b), nil
}

func (w *qwsResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
}

func (w *qwsResponseWriter) Flush() {}

// handleResponsesWebSocket 处理 OpenAI Codex 的 WebSocket 升级请求（快速模式）
// 将 WebSocket 协议转换为内部 HTTP 处理，再通过 WebSocket 帧返回响应
func (q *QuickGateway) handleResponsesWebSocket(w http.ResponseWriter, r *http.Request) {
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

	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return
	}
	acceptKey := quickComputeAcceptKey(key)

	bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	bufrw.WriteString("Upgrade: websocket\r\n")
	bufrw.WriteString("Connection: Upgrade\r\n")
	bufrw.WriteString("Sec-WebSocket-Accept: " + acceptKey + "\r\n")
	bufrw.WriteString("\r\n")
	if err := bufrw.Flush(); err != nil {
		return
	}

	payload, err := quickReadWSFrame(conn)
	if err != nil {
		return
	}

	// @CODEX-DEBUG v0.2.98：记录握手成功 + 入站帧大小 + 前 200 字节（用于定位入站请求形状问题）
	inboundPreview := strings.ReplaceAll(strings.TrimRight(string(payload[:min(len(payload), 200)]), "\n"), "\r", "")
	log.Printf("[CODEX-DEBUG] WS handshake 101 OK, client=%s, inbound_frame_bytes=%d, preview=%q",
		r.RemoteAddr, len(payload), inboundPreview)

	wsReq, err := http.NewRequestWithContext(r.Context(), "POST", r.URL.String(), io.NopCloser(strings.NewReader(string(payload))))
	if err != nil {
		return
	}
	wsReq.Header = r.Header.Clone()
	wsReq.Header.Del("Upgrade")
	wsReq.Header.Del("Connection")
	wsReq.Header.Del("Sec-WebSocket-Key")
	wsReq.Header.Del("Sec-WebSocket-Version")

	wsWriter := &qwsResponseWriter{conn: conn}
	q.handleRequest(wsWriter, wsReq, "responses")
}

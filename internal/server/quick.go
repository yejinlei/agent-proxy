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
func (q *QuickGateway) resolveAlias(clientModel string) (real string, original string, hit bool) {
	if q.aliasFile == nil || clientModel == "" {
		return clientModel, clientModel, false
	}

	rawVal, ok := q.aliasFile.Resolve(clientModel)
	if !ok {
		// 未在映射文件中 → 透传原始模型名（保留到上游验证的入口）
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
func (q *QuickGateway) selectProtocol(ingressProtocol, model string) string {
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

	// ── 协议感知路由：按模型归属选择上游协议（本地变量，不修改 q.info 共享结构） ──
	providerType := q.selectProtocol(ingressProtocol, realModel)
	p := q.getProvider(providerType)

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

	if normalizedIngress == providerType && realModel != "" {
		ctx := context.WithValue(r.Context(), verboseCtxKey{}, vctx)
		stream := quickDetectStream(body)
		if stream {
			// 流式偏好：按上游地址，首次请求并行竞速 SSE vs 非流式，后续直接用胜出方式
			q.streamPreferMu.RLock()
			preferNonStream, tested := q.streamPrefer[q.proxyBaseURL]
			q.streamPreferMu.RUnlock()
			if !tested {
				q.handlePassthroughRace(p, ctx, w, r, realModel, originalModel, aliasHit, startTime, body)
			} else if preferNonStream {
				q.handlePassthroughNonStreamAsSSE(p, ctx, w, r, realModel, originalModel, aliasHit, startTime, body)
			} else {
				q.handlePassthroughStream(p, ctx, w, r, realModel, originalModel, aliasHit, startTime)
			}
		} else {
			q.handlePassthroughNonStream(p, ctx, w, r, realModel, originalModel, aliasHit, startTime)
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
		q.handleStreamRequest(p, ctx, w, downstreamReq, providerTranslator, ingressTranslator, internalReq, startTime)
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
	s := string(body)
	// 尝试 "model": "from" 格式（带空格）
	old := `"model": "` + from + `"`
	if strings.Contains(s, old) {
		return json.RawMessage(strings.Replace(s, old, `"model": "`+to+`"`, 1))
	}
	// 尝试 "model":"from" 格式（无空格）
	old = `"model":"` + from + `"`
	if strings.Contains(s, old) {
		return json.RawMessage(strings.Replace(s, old, `"model":"`+to+`"`, 1))
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

// handlePassthroughNonStream 透传非流式：请求/响应都不翻译，原样转发
func (q *QuickGateway) handlePassthroughNonStream(p provider.Provider, ctx context.Context, w http.ResponseWriter,
	r *http.Request, realModel string, aliasModel string, aliasHit bool, startTime time.Time) {

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

	resp, headers, err := p.Call(callCtx, body, callInfo)
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

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
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

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(q.timeout)*time.Second)
	defer cancel()

	callInfo := makeQuickPassthroughInfo(q.info, realModel)
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		// SSE 头已设置，不能调用 sendError，直接写 SSE 错误事件
		errJSON, _ := json.Marshal(map[string]interface{}{
			"_type":   "error",
			"_status": 400,
			"data":    fmt.Sprintf("read body: %v", err),
		})
		w.Write([]byte("event: error\ndata: "))
		w.Write(errJSON)
		w.Write([]byte("\n\n"))
		flusher.Flush()
		return
	}
	// 命中别名映射时，同步改写请求体中的 model 字段
	if aliasHit && aliasModel != "" {
		body = quickReplaceModelInBody(body, aliasModel, realModel)
	}

	lines, headers, err := p.CallStream(callCtx, body, callInfo)
	if err != nil {
		// SSE 头已设置，不能调用 sendError，直接写 SSE 错误事件
		log.Printf("[passthrough] upstream stream error: %s=%s url=%s body_len=%d err=%v",
			aliasModel, realModel, q.proxyBaseURL, len(body), err)
		errJSON, _ := json.Marshal(map[string]interface{}{
			"_type":   "error",
			"_status": 502,
			"data":    fmt.Sprintf("stream error: %v", err),
		})
		w.Write([]byte("event: error\ndata: "))
		w.Write(errJSON)
		w.Write([]byte("\n\n"))
		flusher.Flush()
		return
	}

	// 透传下游响应头
	for k, v := range headers {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}

	var lastUsage *schema.InternalUsage
	heartbeat := time.NewTicker(2 * time.Second)
	defer heartbeat.Stop()
	// 立即发送首个 SSE 事件，防止客户端在等待上游首个响应时超时断开
	// 必须用 data: 事件而非 SSE 注释（: 开头），因为注释会被客户端忽略不重置超时
	w.Write([]byte("event: ping\ndata: {}\n\n"))
	flusher.Flush()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				goto streamDone
			}
			heartbeat.Reset(2 * time.Second)
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
					// SSE 流已开始后不能再修改 HTTP 状态码，直接写入错误数据
					if errData != "" {
						w.Write([]byte(errData))
					}
					return
				}
			}
			// 累积 usage（用于 -v 日志）
			usage := q.extractUsage(line)
			if usage != nil {
				lastUsage = usage
			}
			writeLine := line
			if aliasHit && aliasModel != "" {
				writeLine = echoAliasInStreamLine(line, aliasModel)
			}
			if _, err := w.Write(writeLine); err != nil {
				log.Printf("[passthrough] stream write error: %s=%s url=%s err=%v",
					aliasModel, realModel, q.proxyBaseURL, err)
				return
			}
			w.Write([]byte("\n\n")) // SSE 协议要求空行分隔事件
			flusher.Flush()
		case <-heartbeat.C:
			// SSE ping 事件：防止客户端因上游思考时间过长而超时断开
			// 必须用 data: 事件而非 SSE 注释，否则客户端不重置流式超时
			w.Write([]byte("event: ping\ndata: {}\n\n"))
			flusher.Flush()
		}
	}
streamDone:

	vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
	vctx.upstreamReq = body
	q.logRequest(vctx, startTime, http.StatusOK, lastUsage, nil)
}

// handlePassthroughRace 首次请求并行竞速 SSE vs 非流式，记录胜出方式
func (q *QuickGateway) handlePassthroughRace(p provider.Provider, ctx context.Context, w http.ResponseWriter,
	r *http.Request, realModel string, aliasModel string, aliasHit bool, startTime time.Time, body []byte) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		q.sendError(w, http.StatusInternalServerError, "streaming not supported", "server does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// SSE 使用客户端 context（客户端断开则取消）
	sseCtx, sseCancel := context.WithCancel(ctx)
	defer sseCancel()
	// 非流式使用独立 context（不受客户端断开影响）
	nsCtx, nsCancel := context.WithTimeout(context.Background(), time.Duration(q.timeout)*time.Second)
	defer nsCancel()

	callInfo := makeQuickPassthroughInfo(q.info, realModel)

	// 准备非流式请求体：去掉 stream 标记
	nsBody := quickRemoveStreamFlag(body)
	if aliasHit && aliasModel != "" {
		nsBody = quickReplaceModelInBody(nsBody, aliasModel, realModel)
	}

	// 准备 SSE 请求体
	sseBody := body
	if aliasHit && aliasModel != "" {
		sseBody = quickReplaceModelInBody(sseBody, aliasModel, realModel)
	}

	type raceResult struct {
		stream    bool
		body      json.RawMessage
		headers   http.Header
		err       error
		lastUsage *schema.InternalUsage
	}
	resultCh := make(chan raceResult, 2)
	sseDone := make(chan struct{})

	// 并行发 SSE 流式
	go func() {
		defer close(sseDone)
		lines, headers, err := p.CallStream(sseCtx, sseBody, callInfo)
		if err != nil {
			resultCh <- raceResult{stream: true, err: err}
			return
		}
		// 透传下游响应头
		for k, v := range headers {
			for _, val := range v {
				w.Header().Add(k, val)
			}
		}
		w.Write([]byte("event: ping\ndata: {}\n\n"))
		flusher.Flush()

		var lastUsage *schema.InternalUsage
		heartbeat := time.NewTicker(2 * time.Second)
		defer heartbeat.Stop()
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					resultCh <- raceResult{stream: true, lastUsage: lastUsage}
					return
				}
				var meta map[string]any
				if json.Unmarshal(line, &meta) == nil {
					if meta["_type"] == "headers" {
						continue
					}
					if meta["_type"] == "error" {
						status, _ := meta["_status"].(float64)
						errData, _ := meta["data"].(string)
						log.Printf("[passthrough] upstream stream error: %s=%s url=%s body_len=%d status=%v err=%s",
							aliasModel, realModel, q.proxyBaseURL, len(sseBody), status, errData)
						resultCh <- raceResult{stream: true, err: fmt.Errorf("upstream error: %s", errData)}
						return
					}
				}
				usage := q.extractUsage(line)
				if usage != nil {
					lastUsage = usage
				}
				writeLine := line
				if aliasHit && aliasModel != "" {
					writeLine = echoAliasInStreamLine(line, aliasModel)
				}
				if _, err := w.Write(writeLine); err != nil {
					log.Printf("[passthrough] stream write error: %s=%s url=%s err=%v",
						aliasModel, realModel, q.proxyBaseURL, err)
					resultCh <- raceResult{stream: true, err: err}
					return
				}
				w.Write([]byte("\n\n"))
				flusher.Flush()
			case <-heartbeat.C:
				// SSE ping 事件：防止客户端因上游响应慢而超时断开
				w.Write([]byte("event: ping\ndata: {}\n\n"))
				flusher.Flush()
			}
		}
	}()

	// 并行发非流式（使用独立 HTTP Client + Transport，避免 SSE 请求取消后影响连接池）
	go func() {
		nsClient := &http.Client{
			Timeout:   time.Duration(q.timeout) * time.Second,
			Transport: &http.Transport{}, // 独立连接池，与 SSE 请求的 DefaultTransport 隔离
		}
		url := p.BuildURL(callInfo, "", false)
		httpReq, err := http.NewRequestWithContext(nsCtx, "POST", url, bytes.NewReader(nsBody))
		if err != nil {
			resultCh <- raceResult{stream: false, err: err}
			return
		}
		reqHeaders := p.DefaultHeaders(callInfo)
		for k, v := range reqHeaders {
			httpReq.Header[k] = v
		}
		log.Printf("[provider] POST %s body_len=%d content_length=%d", url, len(nsBody), httpReq.ContentLength)

		resp, err := nsClient.Do(httpReq)
		if err != nil {
			resultCh <- raceResult{stream: false, err: err}
			return
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			resultCh <- raceResult{stream: false, err: err}
			return
		}

		respHeaders := http.Header{}
		for k, v := range resp.Header {
			respHeaders[k] = v
		}

		if resp.StatusCode >= 400 {
			resultCh <- raceResult{stream: false, err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))}
			return
		}

		// 如果命中别名，回显客户端模型名
		if aliasHit && aliasModel != "" {
			respBody = echoAliasInResponseBody(respBody, aliasModel)
		}
		resultCh <- raceResult{stream: false, body: respBody, headers: respHeaders}
	}()

	// 等待先到者
	result := <-resultCh
	if result.err != nil {
		// 第一个失败，等第二个
		result2 := <-resultCh
		if result2.err != nil {
			log.Printf("[passthrough] race: both failed sse=%v ns=%v: %s=%s",
				result.err, result2.err, aliasModel, realModel)
			// 不调用 sendError（SSE 头已提交），直接写 SSE 错误事件
			w.Write([]byte("event: error\ndata: {\"error\":\"both SSE and non-stream failed\"}\n\n"))
			flusher.Flush()
			return
		}
		result = result2
	}

	// 记录胜出方式（按上游地址）
	q.streamPreferMu.Lock()
	if q.streamPrefer == nil {
		q.streamPrefer = make(map[string]bool)
	}
	q.streamPrefer[q.proxyBaseURL] = !result.stream
	q.streamPreferMu.Unlock()
	log.Printf("[passthrough] race result: %s=%s prefer_nonstream=%v sse_err=%v",
		aliasModel, realModel, !result.stream, result.err)

	if result.stream {
		// SSE 胜出：数据已由 goroutine 写入 w，只需记录日志
		vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
		vctx.upstreamReq = sseBody
		q.logRequest(vctx, startTime, http.StatusOK, result.lastUsage, nil)
	} else {
		// 非流式胜出：包装成 SSE 返回
		// 取消 SSE goroutine，等待其完全退出后再写响应（避免并发写 w）
		sseCancel()
		<-sseDone
		// 透传响应头
		for k, v := range result.headers {
			for _, val := range v {
				w.Header().Add(k, val)
			}
		}
		// 直接写入完整响应（非流式一次返回）
		if _, err := w.Write(result.body); err != nil {
			log.Printf("[passthrough] race write error: %s=%s err=%v", aliasModel, realModel, err)
			return
		}
		vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
		vctx.upstreamReq = nsBody
		q.logRequest(vctx, startTime, http.StatusOK, nil, nil)
	}
}

// handlePassthroughNonStreamAsSSE 非流式调上游，包装成 SSE 返回
func (q *QuickGateway) handlePassthroughNonStreamAsSSE(p provider.Provider, ctx context.Context, w http.ResponseWriter,
	r *http.Request, realModel string, aliasModel string, aliasHit bool, startTime time.Time, body []byte) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		q.sendError(w, http.StatusInternalServerError, "streaming not supported", "server does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	callInfo := makeQuickPassthroughInfo(q.info, realModel)

	// 去掉 stream 标记
	nsBody := quickRemoveStreamFlag(body)
	if aliasHit && aliasModel != "" {
		nsBody = quickReplaceModelInBody(nsBody, aliasModel, realModel)
	}

	respBody, headers, err := p.Call(ctx, nsBody, callInfo)
	if err != nil {
		// SSE 头已设置，不能调用 sendError（会触发 superfluous response.WriteHeader）
		// 直接写入 SSE 错误事件
		log.Printf("[passthrough] nonstream-as-sse error: %s=%s url=%s err=%v",
			aliasModel, realModel, q.proxyBaseURL, err)
		errJSON, _ := json.Marshal(map[string]interface{}{
			"_type":   "error",
			"_status": 502,
			"data":    fmt.Sprintf("upstream error: %v", err),
		})
		w.Write([]byte("event: error\ndata: "))
		w.Write(errJSON)
		w.Write([]byte("\n\n"))
		flusher.Flush()
		return
	}

	// 透传响应头
	for k, v := range headers {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}

	// 回显客户端模型名
	if aliasHit && aliasModel != "" {
		respBody = echoAliasInResponseBody(respBody, aliasModel)
	}

	// 直接写入完整响应
	if _, err := w.Write(respBody); err != nil {
		log.Printf("[passthrough] nonstream-as-sse write error: %s=%s err=%v",
			aliasModel, realModel, err)
		return
	}

	vctx := ctx.Value(verboseCtxKey{}).(verboseCtx)
	vctx.upstreamReq = nsBody
	q.logRequest(vctx, startTime, http.StatusOK, nil, nil)
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

// handleStreamRequest 流式请求
func (q *QuickGateway) handleStreamRequest(p provider.Provider, ctx context.Context, w http.ResponseWriter,
	downstreamReq json.RawMessage, providerTranslator any,
	ingressTranslator translator.CombinedTranslator, internalReq *schema.InternalRequest,
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
		// SSE 头已设置，不能调用 sendError，直接写 SSE 错误事件
		log.Printf("[stream] upstream stream error: %s err=%v", q.proxyBaseURL, err)
		errJSON, _ := json.Marshal(map[string]interface{}{
			"_type":   "error",
			"_status": 502,
			"data":    fmt.Sprintf("stream error: %v", err),
		})
		w.Write([]byte("event: error\ndata: "))
		w.Write(errJSON)
		w.Write([]byte("\n\n"))
		flusher.Flush()
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

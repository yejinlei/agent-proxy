package translator

import (
	"context"
	"encoding/json"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// ═══════════════════════════════════════════════════════════════════════════════
//  翻译器接口
//
//  设计核心：每个协议适配器实现双向翻译。
//  - ToInternal  负责协议 → 中枢模型的转换
//  - FromInternal 负责中枢模型 → 协议的转换（请求和响应）
//  - StreamFromInternal 负责中枢流式事件 → 协议 SSE 的转换
//
//  所有兼容性差异（字段名、角色映射、tool 格式、system prompt 位置、
//  SSE 事件格式、错误码映射）都在这些方法内部显式处理。
// ═══════════════════════════════════════════════════════════════════════════════

// Translator 请求翻译器：协议请求 → InternalRequest
type RequestTranslator interface {
	// Name 返回协议名称
	Protocol() string
	// TranslateRequest 将上游协议请求翻译为内部统一模型。
	// ctx 携带请求路径信息(如 Gemini /v1/models/{model}:generateContent 中的模型名)。
	TranslateRequest(ctx context.Context, rawReq json.RawMessage) (*schema.InternalRequest, error)
}

// ResponseTranslator 响应翻译器：InternalResponse → 协议响应
type ResponseTranslator interface {
	// TranslateResponse 将内部统一响应翻译为协议原生响应
	TranslateResponse(resp *schema.InternalResponse) (json.RawMessage, error)
}

// StreamTranslator 流式翻译器：InternalStreamEvent → 协议 SSE
type StreamTranslator interface {
	// TranslateStream 将内部流式事件翻译为协议 SSE 输出
	// 接收 chan，持续消费直到关闭；回调 fn 接收 (eventData []byte, isDone bool)
	TranslateStream(ctx context.Context, events <-chan schema.InternalStreamEvent, fn func(eventData []byte, isDone bool))
}

// CombinedTranslator 完整翻译器（请求+响应+流式）
type CombinedTranslator interface {
	RequestTranslator
	ResponseTranslator
	StreamTranslator
}

// TranslatorRegistry 翻译器注册表
type TranslatorRegistry struct {
	registry map[string]CombinedTranslator
}

func NewTranslatorRegistry() *TranslatorRegistry {
	return &TranslatorRegistry{
		registry: make(map[string]CombinedTranslator),
	}
}

func (r *TranslatorRegistry) Register(t CombinedTranslator) {
	r.registry[t.Protocol()] = t
}

func (r *TranslatorRegistry) Get(name string) CombinedTranslator {
	return r.registry[name]
}

// TranslateError 将内部错误统一翻译为协议标准错误格式
type ErrorTranslator interface {
	// Name 返回协议名称
	ErrorProtocol() string
	// TranslateError 将内部错误翻译为协议标准错误
	TranslateError(err *schema.StreamError) json.RawMessage
}

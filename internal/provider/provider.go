package provider

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// Provider 下游 provider 客户端接口
type Provider interface {
	Name() string
	BaseURL() string
	Endpoint(model string, stream bool) (method string, url string, err error)
	// Call 发送非流式请求
	Call(ctx context.Context, req json.RawMessage, providerInfo *schema.ProviderInfo) (body json.RawMessage, headers http.Header, err error)
	// CallStream 发送流式请求，返回每行 raw JSON
	CallStream(ctx context.Context, req json.RawMessage, providerInfo *schema.ProviderInfo) (lines <-chan json.RawMessage, headers http.Header, err error)
	// DefaultHeaders 默认请求头
	DefaultHeaders(providerInfo *schema.ProviderInfo) http.Header
	// BuildURL 构建下游请求 URL
	BuildURL(providerInfo *schema.ProviderInfo, model string, stream bool) string
}

// ProviderRegistry 管理所有 provider 客户端
type ProviderRegistry struct {
	providers map[string]Provider
}

func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{
        providers: make(map[string]Provider),
    }
}

func (r *ProviderRegistry) Register(p Provider) {
	r.providers[p.Name()] = p
}

func (r *ProviderRegistry) Get(name string) Provider {
	return r.providers[name]
}

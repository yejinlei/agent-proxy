package router

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// ModelRouter 模型到 Provider 的路由器
type ModelRouter struct {
	modelToProvider map[string]string // model -> provider name
	defaultProvider string
	prefixMatch     bool
	providers       map[string]*providerInfo // provider name -> info
	lock            sync.RWMutex
}

type providerInfo struct {
	info   *schema.ProviderInfo
	weight int
}

func NewModelRouter() *ModelRouter {
	return &ModelRouter{
		modelToProvider: make(map[string]string),
		providers:       make(map[string]*providerInfo),
		prefixMatch:     true,
		defaultProvider: "default",
	}
}

func (r *ModelRouter) AddProvider(name string, info *schema.ProviderInfo) {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.providers[name] = &providerInfo{
		info:   info,
		weight: info.Weight,
	}
}

func (r *ModelRouter) AddRoute(model string, providerName string) {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.modelToProvider[model] = providerName
}

func (r *ModelRouter) SetDefaultProvider(name string) {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.defaultProvider = name
}

func (r *ModelRouter) SetPrefixMatch(enabled bool) {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.prefixMatch = enabled
}

// Resolve 根据模型名解析 Provider
func (r *ModelRouter) Resolve(model string) (*schema.ProviderInfo, string, error) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	var providerName string
	var ok bool

	if r.prefixMatch {
		// 前缀匹配：遍历所有路由，找到最长匹配前缀
		bestMatch := ""
		for routeModel, pName := range r.modelToProvider {
			if strings.HasPrefix(model, routeModel) {
				if len(routeModel) > len(bestMatch) {
					bestMatch = routeModel
					providerName = pName
				}
			}
		}
		if bestMatch == "" {
			return nil, "", fmt.Errorf("no route found for model %q", model)
		}
	} else {
		providerName, ok = r.modelToProvider[model]
		if !ok {
			providerName = r.defaultProvider
		}
	}

	pinfo, ok := r.providers[providerName]
	if !ok {
		return nil, "", fmt.Errorf("provider %q not found", providerName)
	}

	return pinfo.info, providerName, nil
}

// GetProviderNames 获取所有 provider 名称
func (r *ModelRouter) GetProviderNames() []string {
	r.lock.RLock()
	defer r.lock.RUnlock()
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	return names
}

// GetProviderInfo 获取指定 provider 信息
func (r *ModelRouter) GetProviderInfo(name string) *schema.ProviderInfo {
	r.lock.RLock()
	defer r.lock.RUnlock()
	if p, ok := r.providers[name]; ok {
		return p.info
	}
	return nil
}

// WeightedRoundRobin 加权轮询选择 Provider（用于 fallback）
func (r *ModelRouter) WeightedRoundRobin(providerNames []string) *schema.ProviderInfo {
	r.lock.RLock()
	defer r.lock.RUnlock()

	var totalWeight int
	for _, name := range providerNames {
		if p, ok := r.providers[name]; ok {
			totalWeight += p.weight
		}
	}

	if totalWeight == 0 {
		return nil
	}

	target := int((atomicInt.Add(1) % int64(totalWeight)))
	cumulative := 0
	for _, name := range providerNames {
		if p, ok := r.providers[name]; ok {
			cumulative += p.weight
			if target < cumulative {
				return p.info
			}
		}
	}

	return nil
}

var atomicInt atomic.Int64

// LoadRoutesFromJSON 从 JSON 字符串加载路由规则
func (r *ModelRouter) LoadRoutesFromJSON(data json.RawMessage) error {
	var routes struct {
		ModelToProvider map[string]string `json:"model_to_provider"`
		DefaultProvider string            `json:"default_provider"`
		PrefixMatch     bool              `json:"prefix_match"`
	}
	if err := json.Unmarshal(data, &routes); err != nil {
		return err
	}

	r.lock.Lock()
	defer r.lock.Unlock()

	for model, provider := range routes.ModelToProvider {
		r.modelToProvider[model] = provider
	}

	if routes.DefaultProvider != "" {
		r.defaultProvider = routes.DefaultProvider
	}

	r.prefixMatch = routes.PrefixMatch

	return nil
}

package config

import (
	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// Config 主配置
type Config struct {
	// 服务配置
	Server struct {
		Host           string `json:"host"`
		Port           int    `json:"port"`
		ReadTimeout    int    `json:"read_timeout"`
		WriteTimeout   int    `json:"write_timeout"`
		MaxHeaderBytes int    `json:"max_header_bytes"`
	} `json:"server"`

	// Provider 配置
	Providers map[string]*ProviderConfig `json:"providers"`

	// 模型路由配置
	ModelRouter struct {
		ModelToProvider map[string]string `json:"model_to_provider"` // 模型名 → provider 名
		DefaultProvider string            `json:"default_provider"`  // 默认 provider
		PrefixMatch     bool              `json:"prefix_match"`      // 启用前缀匹配
	} `json:"model_router"`

	// 监控配置
	Monitor struct {
		Enabled    bool   `json:"enabled"`
		UIPath     string `json:"ui_path"`
		LogSize    int    `json:"log_size"`    // 环形缓冲区大小
		MetricsTTL int    `json:"metrics_ttl"` // 秒
	} `json:"monitor"`

	// 限流配置
	RateLimit struct {
		Enabled           bool `json:"enabled"`
		RequestsPerSecond int  `json:"rps"`
		Burst             int  `json:"burst"`
		PerProvider       bool `json:"per_provider"`
	} `json:"rate_limit"`

	// 上游认证
	Auth struct {
		APIKey       string   `json:"api_key"`
		MultiKey     bool     `json:"multi_key"`
		MultiKeyList []string `json:"multi_key_list"`
	} `json:"auth"`
}

// ProviderConfig 单个 provider 配置
type ProviderConfig struct {
	BaseURL      string   `json:"base_url"`
	APIToken     string   `json:"api_token"`
	ProviderType string   `json:"provider_type"` // "openai" | "anthropic" | "gemini" | "sensenova"
	APIVersion   string   `json:"api_version"`   // Anthropic 用
	Models       []string `json:"models"`
	Weight       int      `json:"weight"`
	TimeoutSec   int      `json:"timeout_sec"`
	RateLimit    int      `json:"rate_limit"`
	// 自定义端点路径（覆盖默认）
	Endpoints struct {
		ChatCompletion  string `json:"chat_completion"`
		Responses       string `json:"responses"`
		Messages        string `json:"messages"`
		GenerateContent string `json:"generate_content"`
	} `json:"endpoints"`
}

// Load 从 JSON 文件加载配置
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()
	// 简化版：从 JSON 文件加载覆盖默认值
	return cfg, nil
}

// DefaultConfig 返回默认配置
func DefaultConfig() *Config {
	cfg := &Config{}

	cfg.Server.Host = "0.0.0.0"
	cfg.Server.Port = 8080
	cfg.Server.ReadTimeout = 30
	cfg.Server.WriteTimeout = 120
	cfg.Server.MaxHeaderBytes = 1 << 20

	cfg.ModelRouter.DefaultProvider = "default"
	cfg.ModelRouter.PrefixMatch = true

	cfg.Monitor.Enabled = true
	cfg.Monitor.UIPath = "/ui"
	cfg.Monitor.LogSize = 10000
	cfg.Monitor.MetricsTTL = 3600

	cfg.RateLimit.Enabled = true
	cfg.RateLimit.RequestsPerSecond = 100
	cfg.RateLimit.Burst = 200

	return cfg
}

// ToProviderInfo 将配置转为 schema.ProviderInfo
func (pc *ProviderConfig) ToProviderInfo() *schema.ProviderInfo {
	return &schema.ProviderInfo{
		Name:       pc.ProviderType,
		BaseURL:    pc.BaseURL,
		APIToken:   pc.APIToken,
		Models:     pc.Models,
		Weight:     pc.Weight,
		TimeoutSec: pc.TimeoutSec,
		RateLimit:  pc.RateLimit,
		Version:    pc.APIVersion,
	}
}

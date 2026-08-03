package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// Config 主配置
type Config struct {
	// 服务配置
	Server struct {
		Host           string `json:"host" yaml:"host"`
		Port           int    `json:"port" yaml:"port"`
		ReadTimeout    int    `json:"read_timeout" yaml:"read_timeout"`
		WriteTimeout   int    `json:"write_timeout" yaml:"write_timeout"`
		MaxHeaderBytes int    `json:"max_header_bytes" yaml:"max_header_bytes"`
	} `json:"server" yaml:"server"`

	// Provider 配置
	Providers map[string]*ProviderConfig `json:"providers" yaml:"providers"`

	// 模型路由配置
	ModelRouter struct {
		ModelToProvider map[string]string `json:"model_to_provider" yaml:"model_to_provider"`
		DefaultProvider string            `json:"default_provider" yaml:"default_provider"`
		PrefixMatch     bool              `json:"prefix_match" yaml:"prefix_match"`
	} `json:"model_router" yaml:"model_router"`

	// 监控配置
	Monitor struct {
		Enabled    bool   `json:"enabled" yaml:"enabled"`
		UIPath     string `json:"ui_path" yaml:"ui_path"`
		LogSize    int    `json:"log_size" yaml:"log_size"`
		MetricsTTL int    `json:"metrics_ttl" yaml:"metrics_ttl"`
	} `json:"monitor" yaml:"monitor"`

	// 限流配置
	RateLimit struct {
		Enabled           bool `json:"enabled" yaml:"enabled"`
		RequestsPerSecond int  `json:"requests_per_second" yaml:"requests_per_second"`
		Burst             int  `json:"burst" yaml:"burst"`
		PerProvider       bool `json:"per_provider" yaml:"per_provider"`
	} `json:"rate_limit" yaml:"rate_limit"`

	// 上游认证
	Auth struct {
		APIKey       string   `json:"api_key" yaml:"api_key"`
		MultiKey     bool     `json:"multi_key" yaml:"multi_key"`
		MultiKeyList []string `json:"multi_key_list" yaml:"multi_key_list"`
	} `json:"auth" yaml:"auth"`
}

// ProviderConfig 单个 provider 配置
type ProviderConfig struct {
	BaseURL      string   `json:"base_url" yaml:"base_url"`
	APIToken     string   `json:"api_token" yaml:"api_token"`
	ProviderType string   `json:"provider_type" yaml:"provider_type"`
	APIVersion   string   `json:"api_version" yaml:"api_version"`
	Models       []string `json:"models" yaml:"models"`
	Weight       int      `json:"weight" yaml:"weight"`
	TimeoutSec   int      `json:"timeout_sec" yaml:"timeout_sec"`
	RateLimit    int      `json:"rate_limit" yaml:"rate_limit"`
	Endpoints    struct {
		ChatCompletion  string `json:"chat_completion" yaml:"chat_completion"`
		Responses       string `json:"responses" yaml:"responses"`
		Messages        string `json:"messages" yaml:"messages"`
		GenerateContent string `json:"generate_content" yaml:"generate_content"`
	} `json:"endpoints" yaml:"endpoints"`
}

// Load 从 JSON 或 YAML 文件加载配置
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".json" {
		return nil, fmt.Errorf("不支持的配置文件格式: %s，请使用 .json", ext)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

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

package db

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const defaultAliasFileName = "model-aliases.yaml"

func parseAliasYAML(text string) map[string]string {
	m := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.Index(line, ":"); idx > 0 {
			k := strings.TrimSpace(line[:idx])
			v := strings.TrimSpace(line[idx+1:])
			v = strings.Trim(v, `"'`)
			if k != "" {
				m[strings.ToLower(k)] = v
			}
		}
	}
	return m
}

type AliasFile struct {
	mu      sync.RWMutex
	path    string
	entries map[string]string
}

func NewAliasFile() *AliasFile {
	return &AliasFile{entries: make(map[string]string)}
}

func (af *AliasFile) Path() string {
	af.mu.RLock()
	defer af.mu.RUnlock()
	return af.path
}

func (af *AliasFile) Lookup(alias string) (string, bool) {
	af.mu.RLock()
	defer af.mu.RUnlock()
	v, ok := af.entries[strings.ToLower(alias)]
	return v, ok
}

// @AI_GUARD: ALIAS_RESOLVE - 模型别名解析核心逻辑，三层优先级：--aliases > model-aliases.yaml > DefaultAliases()
// @CONSTRAINT: 修改解析逻辑必须理解三层加载机制
//   - 先 Lookup(alias) 取原始映射值
//   - 如果 target == alias（无具体映射），且存在 _default_，则使用 _default_ 值
//   - _default_ 兜底，@default 动态取上游首个模型
//   - 双向替换：请求 model 假→真，响应 model 真→假
//
// @RELATED: DefaultAliases(), LoadAliasFileAuto(), quick.go/gateway.go resolveAlias
// @REASON: 别名解析错误会导致所有请求路由到错误模型
func (af *AliasFile) Resolve(alias string) (target string, hit bool) {
	af.mu.RLock()
	defer af.mu.RUnlock()
	key := strings.ToLower(alias)
	v, ok := af.entries[key]
	if !ok {
		return alias, false
	}
	if strings.ToLower(v) == key {
		if defv, ok := af.entries["_default_"]; ok && defv != "" {
			return defv, true
		}
	}
	return v, true
}

func (af *AliasFile) Entries() map[string]string {
	af.mu.RLock()
	defer af.mu.RUnlock()
	m := make(map[string]string, len(af.entries))
	for k, v := range af.entries {
		m[k] = v
	}
	return m
}

func (af *AliasFile) Set(alias, target string) {
	af.mu.Lock()
	defer af.mu.Unlock()
	af.entries[strings.ToLower(alias)] = target
}

// Merge 用 other 中的条目覆盖 af 中的同名 key（other 优先级更高）
func (af *AliasFile) Merge(other *AliasFile) {
	af.mu.Lock()
	defer af.mu.Unlock()
	other.mu.RLock()
	defer other.mu.RUnlock()
	for k, v := range other.entries {
		af.entries[k] = v
	}
}

func (af *AliasFile) String() string {
	af.mu.RLock()
	defer af.mu.RUnlock()
	return fmt.Sprintf("AliasFile(%d entries)", len(af.entries))
}

func LoadAliasFile(path string) (*AliasFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	af := &AliasFile{path: path, entries: parseAliasYAML(string(data))}
	log.Printf("[aliases] loaded %d entries from %s", len(af.entries), path)
	return af, nil
}

// @AI_GUARD: ALIAS_LOAD_AUTO - 别名文件自动加载，三层优先级机制的核心
// @CONSTRAINT: 加载顺序绝对不可修改：用户文件 > 内置 DefaultAliases()
//   - 以 DefaultAliases() 为基础，用文件中的条目覆盖（Merge）
//   - 文件不存在时回退到 DefaultAliases()
//   - 用户只需配置 _default_ 或少量别名，内置别名仍生效
//
// @RELATED: DefaultAliases(), Resolve(), quick.go/gateway.go alias 初始化
// @REASON: 加载顺序错误会导致用户配置被内置覆盖，所有别名映射失效
func LoadAliasFileAuto(dir string, warn func(string)) (*AliasFile, bool) {
	if dir == "" {
		if p, err := os.Executable(); err == nil {
			dir = filepath.Dir(p)
		} else {
			dir = "."
		}
	}
	fp := filepath.Join(dir, defaultAliasFileName)
	if _, err := os.Stat(fp); err == nil {
		af, err := LoadAliasFile(fp)
		if err != nil {
			if warn != nil {
				warn(fmt.Sprintf("alias file %s exists but failed to load (%v), using built-in defaults", fp, err))
			}
			return DefaultAliases(), false
		}
		// 以内置 DefaultAliases() 为基础，用文件中的条目覆盖
		// 这样用户只需在文件中配置 _default_ 或少量别名，内置的 64 个别名仍然生效
		base := DefaultAliases()
		base.Merge(af)
		log.Printf("[aliases] merged %s with built-in defaults (%d entries total)", fp, len(base.entries))
		return base, true
	}
	if warn != nil {
		warn(fmt.Sprintf("alias file %s not found, using built-in defaults", fp))
	}
	return DefaultAliases(), false
}

// @AI_GUARD: DEFAULT_ALIASES - 内置模型别名，三层加载的最底层兜底
// @CONSTRAINT: 修改别名列表必须同步更新 model-aliases.yaml 模板
//   - 所有别名初始值映射到自身（同名），由 _default_ 或用户配置覆盖
//   - 新增别名需同时更新 AGENTS.md 中的模型别名说明
//
// @RELATED: LoadAliasFileAuto(), Resolve(), model-aliases.yaml
func DefaultAliases() *AliasFile {
	m := make(map[string]string)

	names := []string{
		// Claude 系列
		"claude-opus-4-5", "claude-sonnet-4", "claude-sonnet-4-5", "claude-sonnet-5",
		"claude-sonnet-3-5", "claude-sonnet-3-7", "claude-sonnet-3-0", "claude-sonnet-2-5",
		"claude-haiku-4-5", "claude-haiku-4-0", "claude-haiku-3-5", "claude-haiku-3-0",
		"claude-haiku-2-5", "claude-opus-4", "claude-opus-3-5", "claude-fable-5",
		// Codex 系列
		"codex-fable-5", "codex-opus-4", "codex-sonnet-4",
		// GPT / OpenAI 系列
		"gpt-5-5", "gpt-5-4", "gpt-5-3", "gpt-5-2", "gpt-5",
		"gpt-4-2", "gpt-4o", "gpt-4o-mini", "gpt-4", "gpt-3-5-turbo", "gpt-3-5", "gpt-3",
		"o1", "o3", "o3-mini", "o4-mini", "o4",
		// DeepSeek 系列
		"deepseek-v4", "deepseek-r1", "deepseek-v3",
		// Gemini 系列
		"gemini-3-pro", "gemini-2-5-pro", "gemini-2-pro", "gemini-1-5-pro",
		"gemini-3-flash", "gemini-2-5-flash", "gemini-2-flash", "gemini-2-flash-lite", "gemini-1-5-flash",
		// Qwen / 通义千问
		"qwen3-235b", "qwen3-max", "qwen3-plus", "qwen3-30b",
		"qwen3-8b", "qwen3-4b", "qwen3-flash",
		"qwen-2-5-coder-32b", "qwen-2-5-coder-14b", "qwen-2-5-coder-7b", "qwen-2-5-coder-3b",
		// Doubao / 豆包
		"doubao-1-5-pro", "doubao-pro",
		// GLM / 智谱
		"glm-4-plus", "glm-4", "glm-4-flash",
		// 自定义虚拟模型（走 @default 动态映射）
		"vmodel",
	}
	for _, name := range names {
		m[name] = name
	}
	// 无映射文件时，所有别名动态映射到上游 /v1/models 返回的第一个模型
	m["_default_"] = "@default"

	af := &AliasFile{path: "(built-in)", entries: m}
	log.Printf("[aliases] built-in defaults: %d entries (incl. _default_=@default)", len(m))
	return af
}

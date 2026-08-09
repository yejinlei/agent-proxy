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
		return af, true
	}
	if warn != nil {
		warn(fmt.Sprintf("alias file %s not found, using built-in defaults", fp))
	}
	return DefaultAliases(), false
}

func DefaultAliases() *AliasFile {
	m := make(map[string]string)
	// Claude models
	for _, name := range []string{"claude-opus-4-5", "claude-sonnet-4", "claude-sonnet-4-5", "claude-sonnet-5",
		"claude-sonnet-3-5", "claude-sonnet-3-7", "claude-sonnet-3-0", "claude-sonnet-2-5",
		"claude-haiku-4-5", "claude-haiku-4-0", "claude-haiku-3-5", "claude-haiku-3-0",
		"claude-haiku-2-5", "claude-opus-4", "claude-opus-3-5", "claude-fable-5"} {
		m[name] = name
	}
	// Codex
	for _, name := range []string{"codex-fable-5", "codex-opus-4", "codex-sonnet-4"} {
		m[name] = name
	}
	// GPT / OpenAI
	for _, name := range []string{"gpt-5-5", "gpt-5-4", "gpt-5-3", "gpt-5-2", "gpt-5",
		"gpt-4-2", "gpt-4", "gpt-3-5-turbo", "gpt-3-5", "gpt-3",
		"o1", "o3", "o4-mini", "o4"} {
		m[name] = name
	}
	// DeepSeek
	for _, name := range []string{"deepseek-v4", "deepseek-r1", "deepseek-v3"} {
		m[name] = name
	}
	// Gemini
	for _, name := range []string{"gemini-3-pro", "gemini-3-flash",
		"gemini-2-5-pro", "gemini-2-5-flash",
		"gemini-2-pro", "gemini-2-flash", "gemini-2-flash-lite",
		"gemini-1-5-pro", "gemini-1-5-flash"} {
		m[name] = name
	}
	// Qwen / 通义千问
	for _, name := range []string{"qwen3-235b", "qwen3-30b", "qwen3-8b", "qwen3-4b",
		"qwen3-max", "qwen3-plus", "qwen3-flash",
		"qwen-2-5-coder-32b", "qwen-2-5-coder-14b", "qwen-2-5-coder-7b", "qwen-2-5-coder-3b"} {
		m[name] = name
	}
	// Doubao / 豆包
	for _, name := range []string{"doubao-1-5-pro", "doubao-pro"} {
		m[name] = name
	}
	// GLM / 智谱
	m["glm-4-plus"] = "glm-4-plus"
	m["glm-4"] = "glm-4"
	m["glm-4-flash"] = "glm-4-flash"
	af := &AliasFile{path: "(built-in)", entries: m}
	log.Printf("[aliases] built-in defaults: %d entries", len(m))
	return af
}

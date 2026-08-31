package server

import (
	"testing"
	"github.com/agent-proxy/agent-proxy/internal/db"
)

func TestDefault_WithDBModelsMap(t *testing.T) {
	// 模拟 DB 返回 modelsMap（--db 4 场景）
	modelsMap := map[string][]string{
		"openai":    {"sensenova-6.7-flash-lite", "deepseek-v4-flash", "glm-5.2"},
		"anthropic": {"claude-sonnet-4-5"},
	}
	qg := NewQuickGateway("test", "https://token.sensenova.cn", "sk-test",
		[]string{"openai", "anthropic"}, modelsMap, "", "", 30, "", false, 0)
	qg.SetAliasFile(db.DefaultAliases())

	real, orig, hit := qg.resolveAlias("gpt-4o-mini")
	t.Logf("gpt-4o-mini → real=%q orig=%q hit=%v", real, orig, hit)
	if !hit {
		t.Fatalf("gpt-4o-mini 未命中别名，仍透传原值")
	}
	if real == "gpt-4o-mini" {
		t.Fatalf("gpt-4o-mini 未解析到真实模型名 → 上游 400")
	}
	t.Logf("✅ @default 从 DB modelsMap 解析到 %q", real)
}

func TestDefault_EmptyModelsMap_HTTP_Fails(t *testing.T) {
	// 模拟 DB modelsMap 为空 + HTTP 嗅探失败
	modelsMap := map[string][]string{}
	qg := NewQuickGateway("test", "https://token.sensenova.cn", "sk-test",
		[]string{"openai", "anthropic"}, modelsMap, "", "", 30, "", false, 0)
	qg.SetAliasFile(db.DefaultAliases())

	real, orig, hit := qg.resolveAlias("gpt-4o-mini")
	t.Logf("gpt-4o-mini → real=%q orig=%q hit=%v (HTTP 嗅探应失败)", real, orig, hit)
	if real == "gpt-4o-mini" && !hit {
		t.Logf("❌ modelsMap 空 + HTTP 失败 → 透传原值 → 400")
	}
}

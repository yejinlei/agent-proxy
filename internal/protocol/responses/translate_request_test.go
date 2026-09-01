package responses

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// TestTranslateRequest_FiltersBuiltinTools 验证 v0.2.109 Fix A：
// Codex 经 WS 发来的 tools 含客户端内置工具（tool_search/web_search，无 function.name），
// TranslateRequest 必须丢弃它们，只保留 type=="function" 且 name 非空的 tool，
// 否则上游会收到 function.name 为空的非法定义并报 400 "Invalid request format"。
func TestTranslateRequest_FiltersBuiltinTools(t *testing.T) {
	tr := NewResponsesTranslator()
	// 模拟 Codex 真实 payload：末尾两个是客户端执行的内置工具
	raw := json.RawMessage(`{
		"model":"gpt-5.2",
		"input":[{"type":"message","role":"user","content":"hi"}],
		"stream":true,
		"tools":[
			{"type":"function","name":"read_file","parameters":{"type":"object"}},
			{"type":"tool_search","execution":"client"},
			{"type":"web_search","external_web_access":false}
		]
	}`)
	ir, err := tr.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ir.Tools); got != 1 {
		t.Fatalf("应只保留 1 个 function tool，实际 %d", got)
	}
	if ir.Tools[0].Function == nil || ir.Tools[0].Function.Name != "read_file" {
		t.Fatalf("保留的 tool 应为 read_file，实际 %+v", ir.Tools[0])
	}
	for _, tl := range ir.Tools {
		if tl.Function != nil && tl.Function.Name == "" {
			t.Fatalf("不应存在 function.name 为空的 tool: %+v", tl)
		}
	}
	t.Logf("✅ 内置/空名工具已过滤，保留 tools=%d", len(ir.Tools))
}

// TestTranslateRequest_EmptyInputInjectsUser 验证 v0.2.109 Fix B：
// Codex 连接预热发 input:[] 请求，inputToMessages 转换后无消息，
// TranslateRequest 必须注入一条 user 消息兜底，
// 否则上游只有 system 无 user 会报 400 "No user query found in messages."
func TestTranslateRequest_EmptyInputInjectsUser(t *testing.T) {
	tr := NewResponsesTranslator()
	raw := json.RawMessage(`{
		"model":"gpt-5.2",
		"input":[],
		"instructions":"you are a helpful assistant",
		"stream":false
	}`)
	ir, err := tr.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(ir.Messages) == 0 {
		t.Fatalf("空 input 必须注入 user 消息兜底，实际 messages=0")
	}
	// 注入的必须是 user 角色
	var hasUser bool
	for _, m := range ir.Messages {
		if m.Role == schema.RoleUser {
			hasUser = true
		}
	}
	if !hasUser {
		t.Fatalf("注入的消息必须含 user 角色，实际 %+v", ir.Messages)
	}
	t.Logf("✅ 空 input 已注入 user 消息，messages=%d", len(ir.Messages))
}

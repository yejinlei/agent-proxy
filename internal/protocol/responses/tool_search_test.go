package responses

import (
	"encoding/json"
	"testing"
)

// 入站形状依据 codex-rs/tools/src/tool_spec.rs:29-34 ToolSearch {execution, description, parameters}
// 与 codex-rs/core/src/tools/handlers/tool_search_spec.rs:97-105 的 builder。

func TestToolSearchParams_PassesRealCodexShape(t *testing.T) {
	// 与 Codex 实际发出的形状完全一致（含 execution 与 additionalProperties:false）。
	raw := json.RawMessage(`{
		"type":"tool_search",
		"execution":"client",
		"description":"# Tool discovery\n\nSearches over deferred tool metadata with BM25...",
		"parameters":{
			"type":"object",
			"properties":{"query":{"type":"string"}},
			"required":["query"],
			"additionalProperties":false
		}
	}`)
	p, ok := ToolSearchParams(raw)
	if !ok {
		t.Fatalf("ToolSearchParams: ok=false on valid Codex shape")
	}
	if p.Description == "" {
		t.Errorf("Description empty")
	}
	if p.Parameters == nil {
		t.Fatalf("Parameters nil —— schema 丢了")
	}
	props, ok := p.Parameters["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("Parameters.properties 不是对象, got %T", p.Parameters["properties"])
	}
	if _, ok := props["query"]; !ok {
		t.Errorf("Parameters.properties.query 缺失 —— 降级成 {} 会让模型以为工具无参数")
	}
	req, ok := p.Parameters["required"].([]interface{})
	if !ok || len(req) != 1 || req[0] != "query" {
		t.Errorf("required 未保真: %v", p.Parameters["required"])
	}
	addl, _ := p.Parameters["additionalProperties"].(bool)
	if addl != false {
		t.Errorf("additionalProperties 未保真: %v", p.Parameters["additionalProperties"])
	}
}

func TestToolSearchParams_RejectsEmptyDescription(t *testing.T) {
	// 空 description 合成给 CC 上游会让模型拿到无意义工具说明并可能误调用。
	for name, raw := range map[string][]byte{
		"no_description":    []byte(`{"type":"tool_search","parameters":{"type":"object"}}`),
		"empty_description": []byte(`{"type":"tool_search","description":"","parameters":{"type":"object"}}`),
		"not_json":          []byte(`{`),
		"empty":             []byte(``),
	} {
		if _, ok := ToolSearchParams(raw); ok {
			t.Errorf("%s: 期望拒绝", name)
		}
	}
}

func TestIsToolSearchArgsValid_AcceptsAndCleans(t *testing.T) {
	cases := []struct {
		args   string
		want   map[string]interface{}
	}{
		// 合法 query，无 limit
		{`{"query":"calendar"}`, map[string]interface{}{"query": "calendar"}},
		// 合法 query + 合法 limit，多余字段必须被剥离
		{`{"query":"calendar","limit":5,"bogus":1}`, map[string]interface{}{"query": "calendar", "limit": int64(5)}},
	}
	for _, c := range cases {
		got, cleaned := IsToolSearchArgsValid(c.args)
		if !got {
			t.Errorf("args=%s: 期望 valid", c.args)
			continue
		}
		if len(cleaned) != len(c.want) {
			t.Errorf("args=%s: 字段数 %d != %d, cleaned=%v", c.args, len(cleaned), len(c.want), cleaned)
			continue
		}
		for k, v := range c.want {
			if cleaned[k] != v {
				t.Errorf("args=%s: cleaned[%q]=%v, want %v", c.args, k, cleaned[k], v)
			}
		}
	}
}

func TestIsToolSearchArgsValid_RejectsMalformed(t *testing.T) {
	for name, args := range map[string]string{
		"not_object":     `"raw"`,
		"not_json":       `{`,
		"empty":          ``,
		"no_query":       `{"limit":5}`,
		"query_nonstr":   `{"query":123}`,
		"query_wrongkey": `{"q":"x"}`,
		"query_empty":    `{"query":""}`,
		"query_whitespace": `{"query":"   "}`,
		"query_null":     `{"query":null}`,
	} {
		if ok, _ := IsToolSearchArgsValid(args); ok {
			t.Errorf("%s (args=%q): 期望拒绝", name, args)
		}
	}
}

func TestIsToolSearchArgsValid_DropsBadLimitKeepsQuery(t *testing.T) {
	// Codex 侧 limit 是 Option<usize>，缺失即走默认——这是合法降级路径。
	// limit 非法时丢字段而非拒绝整个调用，避免把可用查询也一并丢弃。
	cases := map[string]string{
		"limit_zero":       `{"query":"x","limit":0}`,
		"limit_negative":   `{"query":"x","limit":-1}`,
		"limit_fractional": `{"query":"x","limit":1.5}`,
		"limit_string":     `{"query":"x","limit":"5"}`,
		"limit_bool":       `{"query":"x","limit":true}`,
		"limit_object":     `{"query":"x","limit":{"a":1}}`,
	}
	for name, args := range cases {
		ok, cleaned := IsToolSearchArgsValid(args)
		if !ok {
			t.Errorf("%s: 期望 valid（limit 降级为缺失）", name)
			continue
		}
		if _, has := cleaned["limit"]; has {
			t.Errorf("%s: limit 未丢弃: %v", name, cleaned["limit"])
		}
		if cleaned["query"] != "x" {
			t.Errorf("%s: query 被改动: %v", name, cleaned["query"])
		}
	}
}

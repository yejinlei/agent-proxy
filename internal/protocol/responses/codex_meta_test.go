package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

// closeTok 用字节码拼出闭合标签。源码里写闭合标签字面量会被工具传输层当成
// 标签剥离（v0.2.134 写这个测试时踩了三次），所以统一走拼接。
func closeTok(name string) string {
	return "<" + string([]byte{47}) + name + ">"
}

// TestDetectToolCallInText 锁住 v0.2.134 新增的"工具调用泄漏进纯文本"检测器。
// 断言重点在"只检测不改行为"：命中只返回描述串，绝不解析成工具调用、也不修改输入。
// 尾部窗口（1500 字符）是刻意设计——这类语法泄漏总在输出末尾，全量扫会漏信号。
func TestDetectToolCallInText(t *testing.T) {
	longClean := strings.Repeat("已完成分析，内容正常。", 120)
	nl := "\n"
	reOpen := "<parameter>"
	reClose := closeTok("parameter")
	closeTag := closeTok("tool_use")

	cases := []struct {
		name     string
		text     string
		wants    []string
		wantMiss bool
	}{
		{name: "空文本", text: "", wantMiss: true},
		{name: "正常正文", text: "文件 a.txt 内容为空。", wantMiss: true},
		{name: "parameter 开标签", text: reOpen + " name=\"cmd\">rg --files",
			wants: []string{"parameter 参数语法"}},
		// 检测器只查开标签 <parameter>，闭标签不在检测范围（真实泄漏总伴随开标签）。
		// 若将来要覆盖闭标签，需同步修改 detectToolCallInText 的 tokens 表。
		{name: "闭标签不在检测范围", text: "执行完毕 " + reClose, wantMiss: true},
		{name: "tool_use 闭标签", text: "}} " + closeTag,
			wants: []string{"tool_use 闭合标签"}},
		{name: "antml 前缀", text: "antml:invoke name=\"apply_patch\">",
			wants: []string{"工具语法前缀"}},
		{name: "function 标签", text: "<function>ls -la",
			wants: []string{"function 声明"}},
		{name: "尾部命中", text: longClean + nl + reOpen,
			wants: []string{"parameter 参数语法"}},
		{name: "头部超窗口不命中", text: reOpen + longClean, wantMiss: true},
		{name: "多模式同时命中", text: reOpen + nl + reClose + "<function>ls",
			wants: []string{"parameter 参数语法", "function 声明"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := detectToolCallInText(c.text)
			if c.wantMiss {
				if got != "" {
					t.Fatalf("应无命中，实际 %q", got)
				}
				return
			}
			for _, want := range c.wants {
				if !strings.Contains(got, want) {
					t.Fatalf("缺少 %q；实际 %q", want, got)
				}
			}
		})
	}
}

// TestCodexSandboxMode 覆盖 client_metadata 的三层嵌套解包：外层 JSON →
// x-codex-turn-metadata 字符串 → 内层 JSON。任何一层解析失败都必须静默返回空串，
// 不能让可观测性代码影响请求处理。
func TestCodexSandboxMode(t *testing.T) {
	full := `{"client_metadata":{"x-codex-turn-metadata":"{\"sandbox_mode\":\"workspace-write\",\"approval_policy\":\"never\",\"working_directory\":\"/root/workspace\"}"}}`
	var outer map[string]json.RawMessage
	if err := json.Unmarshal([]byte(full), &outer); err != nil {
		t.Fatal(err)
	}
	got := codexSandboxMode(outer["client_metadata"])
	// cleanScalarForLog 会剥掉 JSON 标量的成对外层引号，日志里读起来是裸值。
	for _, want := range []string{"sandbox_mode=workspace-write", "approval_policy=never", "workdir=/root/workspace"} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺少 %q；实际 %q", want, got)
		}
	}

	cases := []struct {
		name string
		in   json.RawMessage
		want string
	}{
		// 入参就是 client_metadata 的值本身，不含外层包装。
		{name: "nil 原始值", in: nil, want: ""},
		{name: "空对象", in: json.RawMessage(`{}`), want: ""},
		{name: "无 turn metadata", in: json.RawMessage(`{"other":"x"}`), want: ""},
		{name: "有 client_metadata 但无 turn metadata", in: json.RawMessage(`{"a":"b"}`), want: ""},
		{name: "非法 JSON", in: json.RawMessage(`{"x-codex-turn-metadata":not-json}`), want: ""},
		{name: "turn 是对象而非字符串",
			in: json.RawMessage(`{"x-codex-turn-metadata":{"sandbox_mode":"read-only"}}`),
			want: "sandbox_mode=read-only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := codexSandboxMode(c.in); got != c.want {
				t.Fatalf("输入 %s → %q，期望 %q", string(c.in), got, c.want)
			}
		})
	}
}

// TestResponseRequestRawMetaAndToolChoice 锁定入站顶层控制字段的解析。
// tool_choice / text.format 只观测不落上游，这里确认解析器确实把它们读出来了。
func TestResponseRequestRawMetaAndToolChoice(t *testing.T) {
	in := `{"model":"m","stream":true,"tool_choice":"auto",` +
		`"text":{"format":{"type":"json_schema","name":"codex_output_schema"}},` +
		`"store":false,"tools":[{"type":"function","name":"f"}]}`
	var req ResponseRequest
	if err := json.Unmarshal([]byte(in), &req); err != nil {
		t.Fatal(err)
	}
	if req.ToolChoice == nil {
		t.Fatal("tool_choice 未解析")
	}
	if s := cleanScalarForLog(*req.ToolChoice); s != "auto" {
		t.Fatalf("tool_choice=%s，期望 auto", s)
	}
	if req.Text == nil || req.Text.Format == nil || req.Text.Format.Name != "codex_output_schema" {
		t.Fatalf("text.format 未解析：%+v", req.Text)
	}
	if req.RawMeta == nil {
		t.Fatal("RawMeta 未填充")
	}
	for _, key := range []string{"model", "stream", "tool_choice", "text", "store", "tools"} {
		if _, ok := req.RawMeta[key]; !ok {
			t.Fatalf("RawMeta 缺少 %q：%v", key, req.RawMeta)
		}
	}

	// 无控制字段的请求不能报错，字段应为零值。
	var plain ResponseRequest
	if err := json.Unmarshal([]byte(`{"model":"m"}`), &plain); err != nil {
		t.Fatal(err)
	}
	if plain.ToolChoice != nil || plain.Text != nil {
		t.Fatalf("零值应缺失，得到 %v %v", plain.ToolChoice, plain.Text)
	}

	// 整个 body 非法时仍应报解析错误（RawMeta 解析失败不致命不等于吞掉语法错误）。
	var broken ResponseRequest
	if err := json.Unmarshal([]byte(`{"model":"m","client_metadata":oops}`), &broken); err == nil {
		t.Fatal("body 非法本应报解析错误")
	}
	// 已建模字段类型不匹配也不应阻断其余解析。
	var partial ResponseRequest
	if err := json.Unmarshal([]byte(`{"model":"m"}`), &partial); err != nil {
		t.Fatalf("最小请求不应报错：%v", err)
	}
}

// TestExtractOutputTextMapFallback 确认工具输出的兜底路径不会退化成空串。
// 此前排障担心 extractOutputText 遇到 map 会丢内容，导致上游"误判文件没写好"。
func TestExtractOutputTextMapFallback(t *testing.T) {
	if got := extractOutputText("hello"); got != "hello" {
		t.Fatalf("字符串直通失败：%q", got)
	}
	if got := extractOutputText(nil); got != "" {
		t.Fatalf("nil 应为空串：%q", got)
	}
	got := extractOutputText([]interface{}{
		map[string]interface{}{"type": "output_text", "text": "第一段"},
		map[string]interface{}{"type": "input_text", "text": "第二段"},
	})
	if !strings.Contains(got, "第一段") || !strings.Contains(got, "第二段") {
		t.Fatalf("数组 text 块拼接失败：%q", got)
	}
	if got := extractOutputText(map[string]interface{}{"text": "纯文本输出"}); got != "纯文本输出" {
		t.Fatalf("map text 字段失败：%q", got)
	}
	// 关键断言：兜底走 json.Marshal 保留原始内容，绝不返回空串。
	m := map[string]interface{}{"exit_code": 0, "output": "ls -la done"}
	got = extractOutputText(m)
	if got == "" {
		t.Fatal("兜底返回空串，工具输出会静默丢失")
	}
	if !strings.Contains(got, "ls -la done") || !strings.Contains(got, "exit_code") {
		t.Fatalf("兜底应保留原始内容：%q", got)
	}
}

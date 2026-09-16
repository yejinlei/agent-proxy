package responses

import (
	"strings"
	"testing"
)

// TestExtractLeakedToolNames 锁住 v0.2.149 新增的"泄漏工具名"提取器。
// 断言重点在"只抽名不改行为"：命中只返回名字列表，绝不解析成 function_call、
// 不改写 response.output。尾部 1500 字符窗口与 detectToolCallInText 一致。
// 标签字面量一律用字节码拼接（与 TestDetectToolCallInText 同理由：源码里写闭合
// 标签会被工具传输层剥离，v0.2.134 踩过三次）；双引号同理走 byte(34)。
// @AI_GUARD: RESPONSES_TOOLCALL_IN_TEXT - 提取器输出只喂 END 日志的
// leaked_tools=[...] 字段，禁止据此生成 function_call item 或改写出站体。
// @REASON: END 日志的 toolcall_in_text 只给描述串 + 80 字符上下文，肉眼要翻窗口
// 才知道模型想调什么；leaked_tools=[apply_patch] 一行 grep 直接定位。
// @REASON: v0.2.149 第一版曾从 <parameter> 取名字，但那是"参数名"（command /
// arg_key），不是工具名，会把 exec_command 的参数名当工具名报告。Anthropic 的
// 工具名只出现在 <tool_use name="X"> 的 name 属性里，本测试用例锁住这条边界。
func TestExtractLeakedToolNames(t *testing.T) {
	lt := byte(60)
	gt := byte(62)
	dq := byte(34)
	codexOpen := string([]byte{lt}) + "tool_call" + string([]byte{gt})
	o1Func := string([]byte{lt}) + "function" + string([]byte{gt})
	o1ArgKey := string([]byte{lt}) + "arg_key" + string([]byte{gt})
	o1ArgVal := string([]byte{lt}) + "arg_value" + string([]byte{gt})
	closeArgVal := closeTok("arg_value")
	o1FnClose := closeTok("function")
	// Anthropic 规范形态：工具名只出现在 name 属性里
	tuName := string([]byte{lt}) + "tool_use name=" + string([]byte{dq})
	// 非 name 属性——必须跳过，不能把参数名当工具名
	paramCommand := string([]byte{lt}) + "parameter name=" + string([]byte{dq}) + "command" + string([]byte{dq}) + string([]byte{gt})
	// 参数标签里的 call_id——同样不是工具名
	paramCallID := string([]byte{lt}) + "parameter name=" + string([]byte{dq}) + "call_id" + string([]byte{dq}) + string([]byte{gt})
	longClean := strings.Repeat("正常内容。", 120)
	nl := "\n"

	cases := []struct {
		name string
		text string
		want []string
	}{
		{name: "空文本", text: "", want: []string{}},
		{name: "正常正文无泄漏", text: "已完成分析。", want: []string{}},
		{name: "Codex 开标签裸函数名",
			text: codexOpen + " apply_patch " + nl + "后续内容",
			want: []string{"apply_patch"}},
		{name: "Codex 开标签紧贴无空格",
			text: codexOpen + "exec_command",
			want: []string{"exec_command"}},
		{name: "Codex 开标签后紧跟闭标签",
			text: codexOpen + "apply_patch" + closeTok("tool_call"),
			want: []string{"apply_patch"}},
		{name: "Codex 开标签后空白反引号",
			text: codexOpen + "  ",
			want: []string{}},
		{name: "Codex 开标签加反引号",
			text: codexOpen + "`read_file`",
			want: []string{"read_file"}},
		{name: "Codex 开标签加双引号",
			text: codexOpen + string([]byte{dq}) + "exec_command" + string([]byte{dq}),
			want: []string{"exec_command"}},
		{name: "Codex 两次泄漏去重",
			text: codexOpen + " apply_patch " + nl + codexOpen + " apply_patch " + nl + codexOpen + " exec_command ",
			want: []string{"apply_patch", "exec_command"}},
		{name: "o1 XML 完整形态",
			text: o1Func + o1ArgKey + "name" + closeTok("arg_key") + o1ArgVal + "apply_patch" + closeArgVal + o1FnClose,
			want: []string{"apply_patch"}},
		{name: "o1 XML 独立 arg_value",
			text: o1ArgVal + "exec_command" + closeArgVal,
			want: []string{"exec_command"}},
		{name: "o1 XML 空内容",
			text: o1ArgVal + closeArgVal,
			want: []string{}},
		{name: "Anthropic tool_use name 属性",
			text: tuName + "apply_patch" + string([]byte{dq}) + string([]byte{gt}) + " 正文",
			want: []string{"apply_patch"}},
		{name: "Anthropic 两次泄漏去重",
			text: tuName + "apply_patch" + string([]byte{dq}) + string([]byte{gt}) +
				tuName + "exec_command" + string([]byte{dq}) + string([]byte{gt}),
			want: []string{"apply_patch", "exec_command"}},
		{name: "Anthropic name 属性为空丢弃",
			text: tuName + string([]byte{dq}) + string([]byte{gt}),
			want: []string{}},
		{name: "Anthropic 无 name 属性不抽",
			text: string([]byte{lt}) + "tool_use id=" + string([]byte{dq}) + "toolu_1" + string([]byte{dq}) + string([]byte{gt}),
			want: []string{}},
		{name: "Anthropic tool_use_result 不误判",
			text: closeTok("tool_use_result") + " apply_patch ",
			want: []string{}},
		{name: "Anthropic parameter 参数名不是工具名",
			text: paramCommand + " exec_command " + closeTok("parameter"),
			want: []string{}},
		{name: "Anthropic parameter call_id 不是工具名",
			text: paramCallID + " call_123 " + closeTok("parameter"),
			want: []string{}},
		{name: "尾部窗口只扫最后 1500 字符",
			text: longClean + codexOpen + " apply_patch ",
			want: []string{"apply_patch"}},
		{name: "超长前缀稀释的泄漏仍在窗口内",
			text: longClean + nl + codexOpen + " exec_command " + nl + "尾部说明",
			want: []string{"exec_command"}},
		{name: "名字超过 128 字符丢弃",
			text: codexOpen + " " + strings.Repeat("x", 200),
			want: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractLeakedToolNames(tc.text)
			want := tc.want
			if want == nil {
				want = []string{}
			}
			if len(got) != len(want) {
				t.Fatalf("extractLeakedToolNames(%q) = %v (len %d), want %v (len %d)",
					tc.text, got, len(got), want, len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("extractLeakedToolNames(%q)[%d] = %q, want %q (full got=%v)",
						tc.text, i, got[i], want[i], got)
				}
			}
		})
	}
}

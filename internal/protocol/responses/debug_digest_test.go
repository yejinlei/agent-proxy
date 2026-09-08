package responses

import (
	"strings"
	"testing"
)

// TestResponsesItemDigest_Format 覆盖新增的 [CODEX-DEBUG] input items digest 日志：
// 每种 item 类型的字段选择、空输入、以及 call_id 缺失回退。
//
// 断言重点不在措辞而在"该出现的信号一定出现"：call_id 成对可辨、空 call_id 必须标 SYNTH
// （这是悬空工具结果的唯一可见线索）、工具参数只报长度不报内容。
func TestResponsesItemDigest_Format(t *testing.T) {
	cases := []struct {
		name   string
		items  []InputItem
		expect []string
	}{
		{
			name:   "空输入",
			items:  []InputItem{},
			expect: []string{"(no items)"},
		},
		{
			name:   "message user",
			items:  []InputItem{{Type: "message", Role: "user", Content: []interface{}{map[string]interface{}{"type": "input_text", "text": "读取文件"}}}},
			expect: []string{"0:message role=user", "blocks=input_text", "text_bytes=12"},
		},
		{
			name:  "function_call",
			items: []InputItem{{Type: "function_call", Name: "exec_command", CallID: "call_1", Arguments: `{"cmd":"ls"}`}},
			expect: []string{"0:function_call", "name=exec_command", "call=call_1", "args=12"},
		},
		{
			name:  "function_call 无 call_id",
			items: []InputItem{{Type: "function_call", Name: "exec_command"}},
			expect: []string{"call=SYNTH", "args=0"},
		},
		{
			name:  "function_call_output 字符串",
			items: []InputItem{{Type: "function_call_output", CallID: "call_1", Output: "ok"}},
			expect: []string{"0:function_call_output", "call=call_1", "out=2"},
		},
		{
			name:  "function_call_output 对象",
			items: []InputItem{{Type: "function_call_output", CallID: "call_1", Output: map[string]interface{}{"type": "text", "text": "ok"}}},
			expect: []string{"0:function_call_output", "call=call_1", "out=2"},
		},
		{
			name:   "reasoning 不泄正文",
			items:  []InputItem{{Type: "reasoning", Content: "内部思考"}},
			expect: []string{"0:reasoning"},
		},
		{
			// 一对完整往返：digest 必须能让人看出 call_id 配对
			name:  "完整工具往返",
			items: []InputItem{
				{Type: "function_call", Name: "exec_command", CallID: "call_9", Arguments: `{"cmd":"cat f.txt"}`},
				{Type: "function_call_output", CallID: "call_9", Output: "line1\nline2"},
			},
			expect: []string{"0:function_call", "call=call_9", "1:function_call_output", "call=call_9", "out=11"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := responsesItemDigest(tc.items)
			t.Logf("digest: %s", out)
			for _, frag := range tc.expect {
				if !strings.Contains(out, frag) {
					t.Errorf("digest 缺少片段 %q\n  实际: %s", frag, out)
				}
			}
		})
	}
}

// TestResponsesItemDigest_NoBodyLeak 保证 digest 只报形状：正文/参数原文不得进入日志。
func TestResponsesItemDigest_NoBodyLeak(t *testing.T) {
	secret := "绝密参数值-不应出现在日志中"
	out := responsesItemDigest([]InputItem{
		{Type: "function_call", Name: "exec_command", CallID: "call_1", Arguments: `{"data":"` + secret + `"}`},
		{Type: "function_call_output", CallID: "call_1", Output: secret},
		{Type: "message", Role: "user", Content: secret},
	})
	if strings.Contains(out, secret) {
		t.Errorf("digest 泄露了正文内容: %s", out)
	}
	t.Logf("digest: %s", out)
}

// TestResponsesLogTail 尾部截断：超 160 字符只留尾部，换行压平为单行日志字段。
func TestResponsesLogTail(t *testing.T) {
	if got := responsesLogTail("短"); got != "短" {
		t.Errorf("短文本应保持原样: %q", got)
	}
	long := strings.Repeat("A", 200)
	if got := responsesLogTail(long); len(got) != 160 || !strings.HasPrefix(got, strings.Repeat("A", 160)) {
		t.Errorf("应只保留尾部 160 字符，实际长度 %d", len(got))
	}
	if got := responsesLogTail("第一行\n第二行\r"); strings.ContainsAny(got, "\n\r") {
		t.Errorf("尾部应压平成单行: %q", got)
	}
	if got := responsesLogTail(""); got != "" {
		t.Errorf("空文本应保持为空: %q", got)
	}
}

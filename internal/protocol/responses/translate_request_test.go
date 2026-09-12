package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// TestTranslateRequest_KeepsNonFunctionTools 验证 v0.2.132：
// Codex 经 WS 发来的 tools 含客户端内置工具（tool_search/web_search/custom 等）。
// TranslateRequest 必须**原样保留全部工具**（禁止按 type 白名单丢弃），因为：
//  1. type:"custom" 的 apply_patch 是 Codex 唯一能改文件的 freeform 工具，
//     v0.2.131 之前被白名单整条丢弃，模型被要求用它改文件却拿不到工具；
//  2. ResponseRequest.Tools 用 []json.RawMessage 承载，非 function 工具出站可原样回写，
//     不能因为 CC 上游不认就把它们从 Central Schema 里删掉（下游可能是原生 Responses 上游）。
//
// 真正需要挡的只有「function 工具但 name 为空」——那会让上游报 400 Invalid request format。
// 能力上限（CC 没有等价表达）由 buildCCRequest 按 Function==nil 丢弃并打日志，
// 而不是在入站翻译时静默删除。
func TestTranslateRequest_KeepsNonFunctionTools(t *testing.T) {
	tr := NewResponsesTranslator()
	// 模拟 Codex 真实 payload：末尾两个是客户端执行的内置工具，中间是 freeform 工具
	raw := json.RawMessage(`{
		"model":"gpt-5.2",
		"input":[{"type":"message","role":"user","content":"hi"}],
		"stream":true,
		"tools":[
			{"type":"function","name":"read_file","parameters":{"type":"object"}},
			{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"rl-diff"}},
			{"type":"tool_search","execution":"client"},
			{"type":"web_search","external_web_access":false}
		]
	}`)
	ir, err := tr.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ir.Tools); got != 4 {
		t.Fatalf("必须保留全部 4 个工具（禁止 type 白名单丢弃），实际 %d", got)
	}
	for _, tl := range ir.Tools {
		if tl.Function != nil && tl.Function.Name == "" {
			t.Fatalf("不应存在 function.name 为空的 tool: %+v", tl)
		}
	}
	// @AI_GUARD 回归：custom 工具的 Function 必须为空。
	// v0.2.132 在这里给 IsCustom 工具填了 Function={input:string} + 合成描述，
	// 而 buildCCRequest 对任何 Function!=nil 的工具都会当普通 CC function 原样转发——
	// 结果 (a) custom 工具以 exec_* 形状被 CC 上游当成一个真实函数，
	// (b) 合成描述串到 7 个普通 function 工具上，把它们的 required 数组盖掉。
	// 合成只能发生在 buildCCRequest 一处。
	var customOK bool
	for _, tl := range ir.Tools {
		if tl.IsCustom {
			customOK = true
			if tl.Function != nil {
				t.Fatalf("custom 工具的 Function 必须为空（合成只能发生在 buildCCRequest），实际 %+v", tl)
			}
			if tl.Raw == nil {
				t.Fatalf("custom 工具必须保留 Raw 原始字节（合成名派生 + 原生 Responses 上游原样回写都靠它）")
			}
			if got := toolNameFromRaw(tl.Raw); got != "apply_patch" {
				t.Fatalf("Raw 里必须能取回原工具名 apply_patch，实际 %q", got)
			}
		}
	}
	if !customOK {
		t.Fatalf("type:\"custom\" 的 apply_patch 必须保留且标记 IsCustom，实际 %+v", ir.Tools)
	}
	// 普通 function 工具保持自己的描述与 schema，不得被 freeform 合成描述污染。
	var readOK bool
	for _, tl := range ir.Tools {
		if tl.Function != nil && tl.Function.Name == "read_file" {
			readOK = true
			if strings.Contains(tl.Function.Description, "input") && strings.Contains(tl.Function.Description, "raw text") {
				t.Fatalf("普通 function 工具的描述不得带 freeform 裸文本说明：%q", tl.Function.Description)
			}
		}
	}
	if !readOK {
		t.Fatalf("普通 function 工具 read_file 必须带 Function，实际 %+v", ir.Tools)
	}
	t.Logf("✅ 全部 %d 个工具已保留（含 custom + 客户端内置工具）", len(ir.Tools))
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

// TestTranslateRequest_EmptyUserContentIsFilled 验证 v0.2.136：
// 空 input 注入的是**非空**占位符，且 input 非空但 user 消息内容为空时同样要填。
//
// 上游 SenseNova 有两道连续校验：
//   ① 只有 system 消息 → 400 "Failed to build prompt: No user query found in messages."
//   ② user 消息 content 为空串/纯空白 → 400 "user content required"
//
// v0.2.109 注入 "" 兜住了 ①（agent-proxy-9092.log 09-10 同 body 200）；
// 09-12 上游加严校验 ②，同一字节序列变成 400（agent-proxy-9091.log）。
func TestTranslateRequest_EmptyUserContentIsFilled(t *testing.T) {
	tr := NewResponsesTranslator()

	// 1) 空 input：注入的占位符不能是空串或纯空白
	raw := json.RawMessage(`{"model":"m","input":[],"instructions":"sys"}`)
	ir, err := tr.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(ir.Messages) != 1 {
		t.Fatalf("空 input 应只有 1 条注入消息，实际 %d", len(ir.Messages))
	}
	m := ir.Messages[0]
	if m.Role != schema.RoleUser {
		t.Fatalf("注入消息必须是 user，实际 %s", m.Role)
	}
	var got string
	if err := json.Unmarshal(m.Content, &got); err != nil || strings.TrimSpace(got) == "" {
		t.Fatalf("注入的 user 内容必须非空（空串会让上游 400 \"user content required\"），实际 %q", got)
	}
	if got != prewarmPlaceholder {
		t.Fatalf("注入内容应为固定占位符 %q，实际 %q", prewarmPlaceholder, got)
	}
	t.Logf("✅ 空 input 注入非空占位符: %q", got)
}

// TestTranslateRequest_EmptyUserMessageInHistoryIsFilled 验证 input 非空但
// user 消息内容为空的形态——这是主编码轮次的真实形态（Codex 用空 user item
// 标记"用户没说什么新东西"），影响比 prewarm 大得多：会让整轮编码会话 400。
func TestTranslateRequest_EmptyUserMessageInHistoryIsFilled(t *testing.T) {
	cases := []struct {
		name string
		item string
	}{
		{"content 为空串", `{"type":"message","role":"user","content":""}`},
		{"content 为纯空白", `{"type":"message","role":"user","content":"  "}`},
		{"content 为 nil", `{"type":"message","role":"user"}`},
		{"input_text 块为空串", `{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`},
		{"只有空 text 块", `{"type":"message","role":"user","content":[{"type":"text","text":"   "}]}`},
	}
	tr := NewResponsesTranslator()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := json.RawMessage(fmt.Sprintf(
				`{"model":"m","instructions":"sys","input":[%s,{"type":"message","role":"assistant","content":[{"type":"output_text","text":"thinking"}]}]}`,
				tc.item))
			ir, err := tr.TranslateRequest(context.Background(), raw)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, msg := range ir.Messages {
				if msg.Role != schema.RoleUser {
					continue
				}
				found = true
				var got string
				json.Unmarshal(msg.Content, &got)
				if strings.TrimSpace(got) == "" {
					t.Fatalf("%s → user 内容为空，必须替换成占位符（否则上游 400 \"user content required\"）", tc.name)
				}
				// buildCCRequest 优先用 ContentBlocks 而非 Content，两条出口必须一致：
				// ContentBlocks 里必须恰好是一条占位 text 块，不能残留空块（空块数组会让
				// buildCCRequest 产出空的 CC content，等于没修）。
				if len(msg.ContentBlocks) != 1 || msg.ContentBlocks[0].Type != "text" ||
					msg.ContentBlocks[0].Text != prewarmPlaceholder {
					t.Fatalf("%s → ContentBlocks 必须是单条占位 text 块，实际 %+v", tc.name, msg.ContentBlocks)
				}
				t.Logf("✅ %s → %q", tc.name, got)
			}
			if !found {
				t.Fatalf("%s → 必须存在一条 user 消息", tc.name)
			}
		})
	}
}

// TestTranslateRequest_ImageOnlyUserMessageNotTreatedAsEmpty 是"修 A 坏 B"护栏：
// image-only 消息的 Content 恒为空串，只有 ContentBlocks 里有图片。
// 若空消息判定只看 Content 或把任何非 text 块忽略，就会把图片顶成占位符——
// 图片静默丢失（roundtrip_test.go TestInbound_ImageURL 首次暴露此问题）。
func TestTranslateRequest_ImageOnlyUserMessageNotTreatedAsEmpty(t *testing.T) {
	tr := NewResponsesTranslator()
	raw := json.RawMessage(`{"model":"m","instructions":"sys","input":[
		{"type":"message","role":"user","content":[
			{"type":"input_image","source":{"type":"url","url":"https://example.com/photo.jpg"}}
		]}
	]}`)
	ir, err := tr.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(ir.Messages) != 1 {
		t.Fatalf("应只有 1 条消息，实际 %d", len(ir.Messages))
	}
	m := ir.Messages[0]
	var got string
	json.Unmarshal(m.Content, &got)
	if got == prewarmPlaceholder {
		t.Fatalf("image-only 消息被误判为空并替换成占位符，图片丢失")
	}
	if len(m.ContentBlocks) != 1 || m.ContentBlocks[0].Type != "image" {
		t.Fatalf("image 块被顶掉了，实际 %+v", m.ContentBlocks)
	}
	if m.ContentBlocks[0].URL != "https://example.com/photo.jpg" {
		t.Fatalf("图片 URL 丢失: %+v", m.ContentBlocks[0])
	}
}

// TestTranslateRequest_NonEmptyUserMessageUntouched 确认占位符替换只作用于空内容，
// 正常会话的 user 消息必须逐字节不变——这是防止"修 A 坏 B"的回归护栏。
func TestTranslateRequest_NonEmptyUserMessageUntouched(t *testing.T) {
	tr := NewResponsesTranslator()
	raw := json.RawMessage(`{"model":"m","instructions":"sys","input":[
		{"type":"message","role":"user","content":"修复登录 bug"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"修复登录 bug"}]}
	]}`)
	ir, err := tr.TranslateRequest(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range ir.Messages {
		var got string
		json.Unmarshal(m.Content, &got)
		if got == prewarmPlaceholder {
			t.Fatalf("index %d 的正常 user 消息被替换成占位符", i)
		}
		if got != "修复登录 bug" {
			t.Fatalf("index %d user 内容被篡改: %q", i, got)
		}
	}
}

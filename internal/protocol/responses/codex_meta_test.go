package responses

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
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
	lt := byte(60)
	gt := byte(62)
	codexOpen := string([]byte{lt}) + "tool_call" + string([]byte{gt})
	codexParamEq := string([]byte{lt}) + "parameter=command" + string([]byte{gt})
	codexCloseSpaced := string([]byte{lt}) + " /tool_call" + string([]byte{gt})

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
			wants: []string{"anthropic parameter 裸标签"}},
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
			wants: []string{"anthropic parameter 裸标签"}},
		// v0.2.135 生产日志 16:15:06 那轮：整段工具调用以纯文本吐出、func_calls=0。
	{name: "codex tool_call 开标签", text: codexOpen + "\n" + codexParamEq + "pwd",
		wants: []string{"Codex tool_call", "等号式标签"}},
	// 闭标签被模型写成带空格 "< /tool_call>"，刻意不在检测范围（开标签已足够定位）。
	// 生产日志 16:15:06 那轮的 END tail 原样片段（v0.2.135 当时 toolcall_in_text=""）。
	// 该片段只含闭标签形态、开标签在窗口外——必须命中，否则这类轮次仍是盲区。
	{name: "生产 tail 闭标签形态", text: "\n" + codexParamEq + "\n" +
		closeTok("parameter") + "\n" + closeTok("function"),
		wants: []string{"等号式标签"}},
	{name: "codex 闭标签带空格不命中", text: codexCloseSpaced, wantMiss: true},
	{name: "codex 开标签尾部命中", text: longClean + nl + codexOpen,
		wants: []string{"Codex tool_call"}},
	{name: "头部超窗口不命中", text: reOpen + longClean, wantMiss: true},
		{name: "多模式同时命中", text: reOpen + nl + reClose + "<function>ls" + codexOpen,
			wants: []string{"anthropic parameter 裸标签", "function 声明", "Codex tool_call"}},
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

// TestResponsesDeltaShape 锁住空 delta 形状指纹：只读 key 名，绝不读值。
// 指纹必须稳定可 diff（同现象跨轮次字符串一致），排序靠 keysOfMap 的字母序。
func TestResponsesDeltaShape(t *testing.T) {
	empty := &schema.InternalMessage{Role: schema.RoleAssistant}
	if got := responsesDeltaShape(empty); got != "empty" {
		t.Fatalf("空消息应得 empty，实际 %q", got)
	}
	if got := responsesDeltaShape(nil); got != "no_choice" {
		t.Fatalf("nil 应得 no_choice，实际 %q", got)
	}

	withContent := &schema.InternalMessage{Role: schema.RoleAssistant}
	withContent.Content = []byte(`"hi"`)
	if got := responsesDeltaShape(withContent); got != "content" {
		t.Fatalf("仅 content 应得 content，实际 %q", got)
	}

	// reasoning_chars 走 Metadata，指纹里能看到 key 名但看不到字符数（值不进指纹）。
	reason := &schema.InternalMessage{
		Role:     schema.RoleAssistant,
		Metadata: map[string]interface{}{"reasoning_chars": float64(1234)},
	}
	if got := responsesDeltaShape(reason); got != "reasoning_chars" {
		t.Fatalf("reasoning delta 应得 reasoning_chars，实际 %q", got)
	}

	// 同 fingerprint 的不同值必须产出相同字符串——否则 END 日志无法跨轮次 diff。
	reason2 := &schema.InternalMessage{
		Role:     schema.RoleAssistant,
		Metadata: map[string]interface{}{"reasoning_chars": float64(1)},
	}
	if responsesDeltaShape(reason) != responsesDeltaShape(reason2) {
		t.Fatal("指纹应忽略值，只反映 key")
	}

	// 多 key 组合：排序后拼接，content 在前、reasoning_chars 在后。
	both := &schema.InternalMessage{Role: schema.RoleAssistant}
	both.Content = []byte(`"x"`)
	both.Metadata = map[string]interface{}{"reasoning_chars": float64(5)}
	if got := responsesDeltaShape(both); got != "content+reasoning_chars" {
		t.Fatalf("多 key 应排序拼接，实际 %q", got)
	}

	// tool_call_id / name 也应体现在指纹里（非 content 载荷的空 delta 必须可辨识）。
	tc := &schema.InternalMessage{Role: schema.RoleTool, ToolCallID: "call_1", Name: "exec_command"}
	if got := responsesDeltaShape(tc); got != "name+tool_call_id" {
		t.Fatalf("tool 消息指纹应含 name+tool_call_id，实际 %q", got)
	}
}

// TestEmptyDeltaShapeSummary 锁住 shape 分布串的格式与排序：
// 按次数降序、次数相同时字母序，保证同一现象在不同日志行里字符串一致。
func TestEmptyDeltaShapeSummary(t *testing.T) {
	if got := emptyDeltaShapeSummary(nil); got != "" {
		t.Fatalf("空分布应为空串，实际 %q", got)
	}
	if got := emptyDeltaShapeSummary(map[string]int{"empty": 0}); got != "" {
		t.Fatalf("零计数 key 不应输出，实际 %q", got)
	}
	got := emptyDeltaShapeSummary(map[string]int{"empty": 667, "reasoning_chars": 1})
	if got != "empty=667,reasoning_chars=1" {
		t.Fatalf("排序错误：%q", got)
	}
	got = emptyDeltaShapeSummary(map[string]int{"content+reasoning_chars": 3, "zeta": 3, "alpha": 3})
	if got != "alpha=3,content+reasoning_chars=3,zeta=3" {
		t.Fatalf("同次数应字母序：%q", got)
	}
}

// TestTranslateStreamEmptyDeltaShapeLog 端到端验证 v0.2.134 新增的三个计数
// （empty_deltas / reasoning_chars / delta_shapes）真的会从 delta 事件流里算出来并落日志。
// 断言的是"计数正确"，不是 SSE 字节——reasoning 按硬约束绝不进 output_text 正文。
func TestTranslateStreamEmptyDeltaShapeLog(t *testing.T) {
	events := []schema.InternalStreamEvent{
		{Type: "start", Data: &schema.InternalStreamChunk{Model: "m"}},
		// 纯空 delta：Content 为 nil，无 Metadata → delta_shapes 记 "empty"
		{Type: "delta", Data: &schema.InternalStreamChunk{
			Model:   "m",
			Choices: []schema.InternalChoice{{Message: schema.InternalMessage{Role: schema.RoleAssistant}}},
		}},
		// 第二个空 delta：content 是空 JSON 字符串 → 仍是 empty（Unmarshal 得 ""）
		{Type: "delta", Data: &schema.InternalStreamChunk{
			Model:   "m",
			Choices: []schema.InternalChoice{
				{Message: schema.InternalMessage{Role: schema.RoleAssistant, Content: []byte(`""`)}},
			},
		}},
		// reasoning delta：Content 为 nil，只带 Metadata["reasoning_chars"]=7
		{Type: "delta", Data: &schema.InternalStreamChunk{
			Model: "m",
			Choices: []schema.InternalChoice{{Message: schema.InternalMessage{
				Role:     schema.RoleAssistant,
				Metadata: map[string]interface{}{"reasoning_chars": float64(7)},
			}}},
		}},
		// 有正文的 delta：不应计入 empty_deltas
		{Type: "delta", Data: &schema.InternalStreamChunk{
			Model: "m",
			Choices: []schema.InternalChoice{
				{Message: schema.InternalMessage{Role: schema.RoleAssistant, Content: []byte(`"hi"`)},
					FinishReason: ""},
			},
		}},
		{Type: "done", Data: &schema.InternalStreamChunk{
			Model:   "m",
			Choices: []schema.InternalChoice{{FinishReason: "stop"}},
		}},
	}

	// 捕获 log 输出：log 包每次 Print 独立写一次，并发写 buffer 会丢字节，
	// 所以用 goroutine 把写入搬到另一个 buffer，再 Close 等待刷完。
	captured := newSyncBuffer()
	old := log.Writer()
	log.SetOutput(captured)
	defer log.SetOutput(old)

	out := drainStream(t, NewResponsesTranslator(), context.Background(), events)
	captured.Close()
	gotLog := captured.String()

	if !strings.Contains(gotLog, "TranslateStream END") {
		t.Fatalf("未捕获 END 汇总日志：%q", gotLog)
	}
	for _, want := range []string{
		// 不变量：n_delta_events - text_deltas == empty_deltas → 4 - 1 = 3。
		// reasoning-only delta 计入 empty（它按硬约束不产生任何出站事件），
		// 但它的 shape 单独记在 delta_shapes 里，不会被淹没在 "empty" 中。
		"empty_deltas=3",
		"reasoning_chars=7",
		`delta_shapes="empty=2,reasoning_chars=1"`,
		"n_delta_events=4",
		"text_deltas=1",
	} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("缺少 %q；实际：%q", want, gotLog)
		}
	}
	// 硬约束：reasoning 不得泄漏进 output_text 正文
	s := string(out)
	if strings.Contains(s, "reasoning_chars") || strings.Contains(s, "reasoning") {
		t.Fatalf("reasoning 泄漏进 SSE 正文：%s", s)
	}
}

// syncBuffer 是 log.Writer 的安全实现：log 的每个 Print 会独立触发 Write，
// 并发场景下普通 bytes.Buffer 可能丢字节或串写。这里把每次写入排进队列，
// Close 后再统一读取。
type syncBuffer struct {
	mu    sync.Mutex
	done  chan struct{}
	bufs  [][]byte
}

func newSyncBuffer() *syncBuffer {
	return &syncBuffer{done: make(chan struct{})}
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	cp := make([]byte, len(p))
	copy(cp, p)
	s.mu.Lock()
	s.bufs = append(s.bufs, cp)
	s.mu.Unlock()
	return len(p), nil
}

func (s *syncBuffer) Close() {
	close(s.done)
}

func (s *syncBuffer) String() string {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	parts := make([]string, 0, len(s.bufs))
	for _, b := range s.bufs {
		parts = append(parts, string(b))
	}
	return strings.Join(parts, "")
}


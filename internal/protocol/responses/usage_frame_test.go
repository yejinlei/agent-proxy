package responses

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// runStream 喂入事件序列并收集全部出站 SSE，返回回调收到的字节与回调的调用次数。
func runStream(t *testing.T, events []schema.InternalStreamEvent) (string, int) {
	t.Helper()
	ch := make(chan schema.InternalStreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)

	var sb strings.Builder
	var n int
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		(&ResponsesTranslator{}).TranslateStream(ctx, ch, func(data []byte, isDone bool) {
			sb.Write(data)
			n++
			if isDone {
				close(done)
			}
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("TranslateStream 未在 5s 内收尾（usage-only 尾帧可能卡住收尾路径）\n已输出:\n%s", sb.String())
	}
	return sb.String(), n
}

// lastDataLine 返回 s 中最后一条含 marker 的 SSE data 行内容。
func lastDataLine(s, marker string) string {
	best := ""
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") || !strings.Contains(line, marker) {
			continue
		}
		best = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	}
	return best
}

// TestTranslateStream_UsageOnlyFrame 用感诺线上真实的帧序列回归：
//   delta → finish 帧 → usage-only 尾帧（choices:[] + usage）
//
// @AI_GUARD: RESPONSES_USAGE_ONLY_FRAME - 流式 usage 的唯一来源是 choices:[] 的独立尾帧
// @REASON: 生产上 usage 恒为 &{0 0 0 0 0}，三条链路叠加导致：
//  1. buildCCRequest 按上游类型把 stream_options 置 nil（归因写错，真凶是 role:"developer"）
//  2. quick.go/gateway.go 用 `len(ccChunk.Choices) == 0` 把尾帧 continue 掉
//  3. 本翻译器 case "done" 拿到 finish 帧后直接 sendCompleted()+return，尾帧永远收不到
//
// Codex 的 auto_compact_scope_tokens 严格等于 total_usage_tokens（实测 223/223 精确相等），
// 所以 usage=0 → 阈值 0 → 自动压缩永不触发 → 历史无界增长 → 撞 body 上限。
// 三条链路任一未修，本测试都会失败。
func TestTranslateStream_UsageOnlyFrame(t *testing.T) {
	events := []schema.InternalStreamEvent{
		{Type: "delta", Data: &schema.InternalStreamChunk{
			ID:    "chunk_1",
			Model: "glm-5.2",
			Choices: []schema.InternalChoice{{Index: 0, Message: schema.InternalMessage{
				Role:    schema.RoleAssistant,
				Content: json.RawMessage(`"hello"`),
			}}},
		}},
		{Type: "done", Data: &schema.InternalStreamChunk{
			ID:    "chunk_2",
			Model: "glm-5.2",
			Choices: []schema.InternalChoice{{Index: 0, FinishReason: "stop", Message: schema.InternalMessage{
				Role:    schema.RoleAssistant,
				Content: json.RawMessage(`"world"`),
			}}},
		}},
		// usage-only 尾帧：choices 为空，usage 是唯一真实值
		{Type: "usage", Data: &schema.InternalStreamChunk{
			ID:    "chunk_3",
			Model: "glm-5.2",
			Usage: &schema.InternalUsage{PromptTokens: 727, CompletionTokens: 133, TotalTokens: 860},
		}},
	}

	s, _ := runStream(t, events)

	if n := strings.Count(s, `"type":"response.completed"`); n != 1 {
		t.Fatalf("response.completed 应恰好 1 条（completedSent 幂等），实际 %d 条\n%s", n, s)
	}

	completed := lastDataLine(s, "response.completed")
	if completed == "" {
		t.Fatalf("未找到 response.completed 的 data 行\n%s", s)
	}

	var ev struct {
		Response struct {
			Status string `json:"status"`
			Usage  *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
				TotalTokens  int `json:"total_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(completed), &ev); err != nil {
		t.Fatalf("response.completed 解析失败: %v\n%s", err, completed)
	}
	if ev.Response.Usage == nil {
		t.Fatalf("response.completed 缺少 usage（Codex 依赖它触发自动压缩）\n%s", completed)
	}
	if ev.Response.Usage.InputTokens != 727 || ev.Response.Usage.OutputTokens != 133 || ev.Response.Usage.TotalTokens != 860 {
		t.Fatalf("usage 未取到 usage-only 尾帧的真值，实际 input=%d output=%d total=%d（期望 727/133/860）\n%s",
			ev.Response.Usage.InputTokens, ev.Response.Usage.OutputTokens, ev.Response.Usage.TotalTokens, completed)
	}
	t.Logf("✅ response.completed 携带真实 usage input=%d output=%d total=%d status=%s",
		ev.Response.Usage.InputTokens, ev.Response.Usage.OutputTokens, ev.Response.Usage.TotalTokens, ev.Response.Status)
}

// TestTranslateStream_NoFinishFrame_UsageOnly 覆盖退化形态：
// 上游不发 finish 帧，只发 delta 后直接给 usage-only 帧。
// case "done" 不会执行，必须由 case "usage" 补发终态。
func TestTranslateStream_NoFinishFrame_UsageOnly(t *testing.T) {
	events := []schema.InternalStreamEvent{
		{Type: "delta", Data: &schema.InternalStreamChunk{
			Choices: []schema.InternalChoice{{Index: 0, Message: schema.InternalMessage{
				Role:    schema.RoleAssistant,
				Content: json.RawMessage(`"hi"`),
			}}},
		}},
		{Type: "usage", Data: &schema.InternalStreamChunk{
			Usage: &schema.InternalUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
		}},
	}

	s, _ := runStream(t, events)

	if n := strings.Count(s, `"type":"response.completed"`); n != 1 {
		t.Fatalf("无 finish 帧路径也必须发出 1 条 response.completed，实际 %d 条\n%s", n, s)
	}
	completed := lastDataLine(s, "response.completed")
	if !strings.Contains(completed, `"total_tokens":120`) {
		t.Fatalf("无 finish 帧路径未取到 usage-only 尾帧的 usage\n%s", completed)
	}
	t.Logf("✅ 无 finish 帧路径由 case \"usage\" 补发终态并携带 usage")
}

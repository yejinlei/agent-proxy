package anthropic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// runAnthropicStream 喂入事件序列并收集全部出站 SSE，返回拼接后的字节串。
// isDone 触发即视为收尾完成；5s 上限用来捕获「收尾路径挂住」这类回归。
func runAnthropicStream(t *testing.T, events []schema.InternalStreamEvent) string {
	t.Helper()
	ch := make(chan schema.InternalStreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)

	var sb strings.Builder
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		(&AnthropicTranslator{}).TranslateStream(ctx, ch, func(data []byte, isDone bool) {
			sb.Write(data)
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
	return sb.String()
}

// lastDataLine 返回 s 中最后一条含 marker 的 SSE data 行内容（已去掉 "data:" 前缀）。
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

type stopShape struct {
	Type         string `json:"type"`
	StopReason   string `json:"stop_reason"`
	MessageDelta *struct {
		StopReason string `json:"stop_reason"`
	} `json:"message_delta"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// parseStopReason 从 message_delta 事件里取 stop_reason。
func parseStopReason(t *testing.T, raw string) string {
	t.Helper()
	var ev stopShape
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("message_delta 解析失败: %v\n%s", err, raw)
	}
	if ev.MessageDelta != nil {
		return ev.MessageDelta.StopReason
	}
	return ""
}

// TestAnthropicStream_UsageOnlyFrame 用感诺线上真实帧序列回归：
//
//	delta → finish 帧 → usage-only 尾帧（choices:[] + usage）
//
// @AI_GUARD: ANTHROPIC_USAGE_ONLY_FRAME - 流式 usage 的唯一来源是 choices:[] 的独立尾帧
// @REASON: 生产上 Claude Code 侧 usage 恒 0，两条链路叠加导致：
//  1. quick.go/gateway.go 用 `len(ccChunk.Choices) == 0` 把尾帧 continue 掉
//     （v0.2.144 已改为发 Type:"usage" 事件）
//  2. 本翻译器 case "done" 拿到 finish 帧后直接发完终态就 return，尾帧永远收不到
//
// 旧实现还有第三个问题：usage==nil 时跳过 message_stop 只调 fn(nil,true)，
// 而 OpenAI 系上游的 finish 帧不带 usage，所以生产上 Claude Code 能收到
// message_delta 却收不到 message_stop，违反 Anthropic 事件生命周期。
func TestAnthropicStream_UsageOnlyFrame(t *testing.T) {
	events := []schema.InternalStreamEvent{
		{Type: "delta", Data: &schema.InternalStreamChunk{
			ID:    "chunk_1",
			Model: "sensenova-6.8-flash-lite",
			Choices: []schema.InternalChoice{{Index: 0, Message: schema.InternalMessage{
				Role:    schema.RoleAssistant,
				Content: json.RawMessage(`"hello"`),
			}}},
		}},
		// finish 帧：不带 usage（感诺把 usage 放在独立尾帧）
		{Type: "done", Data: &schema.InternalStreamChunk{
			ID:    "chunk_2",
			Model: "sensenova-6.8-flash-lite",
			Choices: []schema.InternalChoice{{Index: 0, FinishReason: "stop", Message: schema.InternalMessage{
				Role:    schema.RoleAssistant,
				Content: json.RawMessage(`"world"`),
			}}},
		}},
		// usage-only 尾帧：choices 为空，usage 是唯一真实值
		{Type: "usage", Data: &schema.InternalStreamChunk{
			ID:    "chunk_3",
			Model: "sensenova-6.8-flash-lite",
			Usage: &schema.InternalUsage{PromptTokens: 727, CompletionTokens: 133, TotalTokens: 860},
		}},
	}

	s := runAnthropicStream(t, events)

	if n := strings.Count(s, `"type":"message_stop"`); n != 1 {
		t.Fatalf("message_stop 应恰好 1 条（terminalSent 幂等），实际 %d 条\n%s", n, s)
	}
	if n := strings.Count(s, `"type":"message_delta"`); n != 1 {
		t.Fatalf("message_delta 应恰好 1 条，实际 %d 条\n%s", n, s)
	}
	if n := strings.Count(s, `"type":"content_block_stop"`); n != 1 {
		t.Fatalf("content_block_stop 应恰好 1 条，实际 %d 条\n%s", n, s)
	}

	stop := lastDataLine(s, "message_stop")
	if stop == "" {
		t.Fatalf("未找到 message_stop（Claude Code 校验完整事件生命周期）\n%s", s)
	}
	var ev stopShape
	if err := json.Unmarshal([]byte(stop), &ev); err != nil {
		t.Fatalf("message_stop 解析失败: %v\n%s", err, stop)
	}
	if ev.Usage == nil {
		t.Fatalf("message_stop 缺少 usage（客户端依赖它统计上下文占用）\n%s", stop)
	}
	if ev.Usage.InputTokens != 727 || ev.Usage.OutputTokens != 133 || ev.Usage.TotalTokens != 860 {
		t.Fatalf("usage 未取到 usage-only 尾帧的真值，实际 input=%d output=%d total=%d（期望 727/133/860）\n%s",
			ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.TotalTokens, stop)
	}
	if got := parseStopReason(t, lastDataLine(s, "message_delta")); got != "end_turn" {
		t.Fatalf("stop_reason 期望 end_turn（finish_reason=stop），实际 %q", got)
	}

	// 事件顺序：message_delta 必须在 message_stop 之前
	if i := strings.Index(s, "message_delta"); i >= 0 && strings.Index(s, "message_stop") >= 0 &&
		i > strings.Index(s, "message_stop") {
		t.Fatalf("message_delta 必须在 message_stop 之前\n%s", s)
	}
	t.Logf("✅ message_stop 携带真实 usage input=%d output=%d total=%d",
		ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.TotalTokens)
}

// TestAnthropicStream_NoUsageTrailingFrame 覆盖上游不发 usage 尾帧的情形：
// 必须仍然发送 message_stop（旧实现会静默跳过）。
func TestAnthropicStream_NoUsageTrailingFrame(t *testing.T) {
	events := []schema.InternalStreamEvent{
		{Type: "delta", Data: &schema.InternalStreamChunk{
			Choices: []schema.InternalChoice{{Index: 0, Message: schema.InternalMessage{
				Role:    schema.RoleAssistant,
				Content: json.RawMessage(`"hi"`),
			}}},
		}},
		{Type: "done", Data: &schema.InternalStreamChunk{
			Choices: []schema.InternalChoice{{Index: 0, FinishReason: "stop"}},
		}},
	}

	s := runAnthropicStream(t, events)
	if n := strings.Count(s, `"type":"message_stop"`); n != 1 {
		t.Fatalf("无 usage 尾帧也必须发送 message_stop（旧实现会跳过），实际 %d 条\n%s", n, s)
	}
	t.Logf("✅ 无 usage 尾帧时仍发送 message_stop")
}

// TestAnthropicStream_NoFinishFrame_UsageOnly 覆盖退化形态：
// 上游不发 finish 帧，只发 delta 后直接给 usage-only 帧。case "done" 不执行，
// 必须由 case "usage" 补发终态。
func TestAnthropicStream_NoFinishFrame_UsageOnly(t *testing.T) {
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

	s := runAnthropicStream(t, events)
	if n := strings.Count(s, `"type":"message_stop"`); n != 1 {
		t.Fatalf("无 finish 帧路径也必须发出 1 条 message_stop，实际 %d 条\n%s", n, s)
	}
	stop := lastDataLine(s, "message_stop")
	if !strings.Contains(stop, `"total_tokens":120`) {
		t.Fatalf("无 finish 帧路径未取到 usage-only 尾帧的 usage\n%s", stop)
	}
	t.Logf("✅ 无 finish 帧路径由 case \"usage\" 补发终态并携带 usage")
}

// TestAnthropicStream_ChannelClose_NoUsage 覆盖最退化形态：
// 上游只发 delta 就关闭 channel（既不发 finish 也不发 usage）。
// 关闭分支的有界等待会超时，然后必须收尾发出 message_stop。
func TestAnthropicStream_ChannelClose_NoUsage(t *testing.T) {
	events := []schema.InternalStreamEvent{
		{Type: "delta", Data: &schema.InternalStreamChunk{
			Choices: []schema.InternalChoice{{Index: 0, Message: schema.InternalMessage{
				Role:    schema.RoleAssistant,
				Content: json.RawMessage(`"hi"`),
			}}},
		}},
	}

	start := time.Now()
	s := runAnthropicStream(t, events)
	elapsed := time.Since(start)

	if n := strings.Count(s, `"type":"message_stop"`); n != 1 {
		t.Fatalf("channel 关闭路径也必须发出 message_stop，实际 %d 条\n%s", n, s)
	}
	// 收尾延迟必须是有界等待本身（100ms 级），绝不能无限挂住
	if elapsed > 3*time.Second {
		t.Fatalf("channel 关闭路径收尾过慢（%v），有界等待可能失效", elapsed)
	}
	t.Logf("✅ channel 关闭路径收尾于 %v", elapsed)
}

// TestAnthropicStream_MaxTokensStopReason 确认有界等待改动未破坏 finish_reason 映射。
func TestAnthropicStream_MaxTokensStopReason(t *testing.T) {
	events := []schema.InternalStreamEvent{
		{Type: "delta", Data: &schema.InternalStreamChunk{
			Choices: []schema.InternalChoice{{Index: 0, Message: schema.InternalMessage{
				Role:    schema.RoleAssistant,
				Content: json.RawMessage(`"hi"`),
			}}},
		}},
		{Type: "done", Data: &schema.InternalStreamChunk{
			Choices: []schema.InternalChoice{{Index: 0, FinishReason: "length"}},
		}},
	}
	s := runAnthropicStream(t, events)
	if got := parseStopReason(t, lastDataLine(s, "message_delta")); got != "max_tokens" {
		t.Fatalf("finish_reason=length 应映射为 max_tokens，实际 %q", got)
	}
	t.Logf("✅ finish_reason=length → max_tokens")
}

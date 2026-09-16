package responses

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// runStreamDelayed 把 events 分成两段：先发前段、sleep delay、再发后段。
// 用于制造 lastRealContentAt 与 done 之间真实的静默窗口，
// 触发 noContentStallTimeout 判死。
func runStreamDelayed(t *testing.T, first []schema.InternalStreamEvent, delay time.Duration, rest []schema.InternalStreamEvent) string {
	t.Helper()
	ch := make(chan schema.InternalStreamEvent, len(first)+len(rest))

	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		for _, e := range first {
			ch <- e
		}
		// 让 TranslateStream 消费完 first 段后再 sleep，
		// 否则 sleep 期间 TranslateStream 还没开始消费、lastRealContentAt 尚未建立。
		time.Sleep(delay)
		for _, e := range rest {
			ch <- e
		}
		close(ch)
	}()

	var sb strings.Builder
	go func() {
		(&ResponsesTranslator{}).TranslateStream(ctx, ch, func(data []byte, isDone bool) {
			sb.Write(data)
			if isDone {
				close(done)
			}
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("TranslateStream 未在 5s 内收尾\n已输出:\n%s", sb.String())
	}
	return sb.String()
}

// TestTranslateStream_NoContentStall_FiresFailed 是 no-content stall 检测的主路径。
//
// @AI_GUARD: RESPONSES_NO_CONTENT_STALL - 必须发 response.failed 而非 response.completed
// @REASON: agent-proxy-9091.log 2026-09-15 20:13:39 — 5 分钟内 8925 个空 delta（~3/秒
//
//	的 keepalive），text_chars=0 reasoning_chars=0 func_calls=0，代理在 q.timeout（300s）
//	后发合法 response.completed（finish_reason:"stop"），Codex 判 ThreadIdleCause::Completed
//	→ 静默空回合，stream_max_retries=5 全程死代码。修完之后 60s（单测 5ms）即判死并发
//	response.failed（error.code=upstream_timeout），Codex 映射 ApiError::Retryable →
//	触发 stream_max_retries=5 的重试循环。
//
// @CONSTRAINT: 断言必须同时锁定：
//
//	(1) 出现 response.failed 恰好一次
//	(2) 不出现 response.completed（这是本修复的核心——不能走 ThreadIdleCause::Completed）
//	(3) response.failed 的 error.code 不在 Codex 非重试白名单里（走 ApiError::Retryable）
//
// @RELATED: TranslateStream 内 RESPONSES_NO_CONTENT_STALL 分支
func TestTranslateStream_NoContentStall_FiresFailed(t *testing.T) {
	orig := noContentStallTimeout
	noContentStallTimeout = 5 * time.Millisecond
	defer func() { noContentStallTimeout = orig }()

	// 30 个空 delta，模拟日志里 8925 空 delta 的最小复现。
	// Content 为 nil、无 ToolCalls、无 Reasoning → 三处 lastRealContentAt 都不更新。
	first := make([]schema.InternalStreamEvent, 30)
	for i := range first {
		first[i] = schema.InternalStreamEvent{
			Type: "delta",
			Data: &schema.InternalStreamChunk{
				ID:    "chunk_empty",
				Model: "glm-5.2",
				Choices: []schema.InternalChoice{{
					Index:   0,
					Message: schema.InternalMessage{Role: schema.RoleAssistant},
				}},
			},
		}
	}

	rest := []schema.InternalStreamEvent{{
		Type: "done",
		Data: &schema.InternalStreamChunk{
			ID:    "chunk_done",
			Model: "glm-5.2",
			Choices: []schema.InternalChoice{{
				Index:        0,
				FinishReason: "stop",
				Message:      schema.InternalMessage{Role: schema.RoleAssistant},
			}},
		},
	}}

	s := runStreamDelayed(t, first, 50*time.Millisecond, rest)

	if n := strings.Count(s, `"type":"response.failed"`); n != 1 {
		t.Fatalf("response.failed 应恰好 1 条（no-content stall 判死），实际 %d 条\n%s", n, s)
	}
	if strings.Contains(s, `"type":"response.completed"`) {
		t.Fatalf("命中 no-content stall 后必须不发 response.completed\n"+
			"（发 completed 无论 status 都是 Ok(ResponseEvent::Completed) → ThreadIdleCause::Completed\n"+
			"→ Codex 静默成功、重试不触发）\n%s", s)
	}
	if !strings.Contains(s, `"code":"upstream_timeout"`) {
		t.Fatalf("response.failed 的 error.code 必须是 upstream_timeout\n"+
			"（避开关到 Codex 非重试白名单：context_length_exceeded / insufficient_quota / "+
			"usage_not_included / cyber_policy / misalignment_policy_violation / "+
			"invalid_prompt / bio_policy / server_is_overloaded / rate_limit_exceeded）\n%s", s)
	}
}

// TestTranslateStream_NoContentStall_DoesNotFireWithRealContent 反向验证：有真实内容时不能误判。
//
// @CONSTRAINT: 只要 accumulatedText.Len() > 0 就必须走 response.completed 路径。
//
//	这是 60s 阈值 + 三条件联合判断（nDeltaEvents > 0 && text==0 && reasoning==0 && func_calls==0）
//	里的关键防误杀分支。
func TestTranslateStream_NoContentStall_DoesNotFireWithRealContent(t *testing.T) {
	orig := noContentStallTimeout
	noContentStallTimeout = 5 * time.Millisecond
	defer func() { noContentStallTimeout = orig }()

	// 空 delta + 真实内容 delta 都在 first 段（真实内容会重置 lastRealContentAt），
	// 然后静默 50ms（超过阈值），done 触发收尾时 text 已非空 → 不判死。
	first := []schema.InternalStreamEvent{
		{Type: "delta", Data: &schema.InternalStreamChunk{
			ID:    "chunk_empty",
			Model: "glm-5.2",
			Choices: []schema.InternalChoice{{
				Message: schema.InternalMessage{Role: schema.RoleAssistant},
			}},
		}},
		{Type: "delta", Data: &schema.InternalStreamChunk{
			ID:    "chunk_real",
			Model: "glm-5.2",
			Choices: []schema.InternalChoice{{
				Message: schema.InternalMessage{
					Role:    schema.RoleAssistant,
					Content: json.RawMessage(`"实际回答"`),
				},
			}},
		}},
	}
	rest := []schema.InternalStreamEvent{{
		Type: "done",
		Data: &schema.InternalStreamChunk{
			ID:    "chunk_done",
			Model: "glm-5.2",
			Choices: []schema.InternalChoice{{
				FinishReason: "stop",
				Message:      schema.InternalMessage{Role: schema.RoleAssistant},
			}},
		},
	}}

	s := runStreamDelayed(t, first, 50*time.Millisecond, rest)

	if strings.Contains(s, `"type":"response.failed"`) {
		t.Fatalf("有真实内容时不得误判 no-content stall\n%s", s)
	}
	if n := strings.Count(s, `"type":"response.completed"`); n != 1 {
		t.Fatalf("有真实内容时应走 response.completed 路径，实际 %d 条\n%s", n, s)
	}
}

// TestTranslateStream_NoContentStall_ReasoningCountsAsRealContent reasoning-only 模型
// （o1 / DeepSeek R1）几十秒只有思考、无 text 是正常形态，不能误杀。
//
// @CONSTRAINT: reasoningChars > 0 即重置 lastRealContentAt。
func TestTranslateStream_NoContentStall_ReasoningCountsAsRealContent(t *testing.T) {
	orig := noContentStallTimeout
	noContentStallTimeout = 5 * time.Millisecond
	defer func() { noContentStallTimeout = orig }()

	// reasoning delta 重置 lastRealContentAt，之后静默 50ms，
	// reasoningChars > 0 使 no-content stall 判断为 false。
	first := []schema.InternalStreamEvent{{
		Type: "delta",
		Data: &schema.InternalStreamChunk{
			ID:    "chunk_r1",
			Model: "glm-5.2",
			Choices: []schema.InternalChoice{{
				Message: schema.InternalMessage{
					Role:     schema.RoleAssistant,
					Metadata: map[string]any{"reasoning_chars": float64(50)},
				},
			}},
		},
	}}
	rest := []schema.InternalStreamEvent{{
		Type: "done",
		Data: &schema.InternalStreamChunk{
			ID:    "chunk_done",
			Model: "glm-5.2",
			Choices: []schema.InternalChoice{{
				FinishReason: "stop",
				Message:      schema.InternalMessage{Role: schema.RoleAssistant},
			}},
		},
	}}

	s := runStreamDelayed(t, first, 50*time.Millisecond, rest)

	if strings.Contains(s, `"type":"response.failed"`) {
		t.Fatalf("reasoning-only 回合不得误判 no-content stall（reasoningChars 已 > 0）\n%s", s)
	}
	if n := strings.Count(s, `"type":"response.completed"`); n != 1 {
		t.Fatalf("reasoning-only 回合应走 response.completed，实际 %d 条\n%s", n, s)
	}
}

// TestTranslateStream_NoContentStall_DefaultThreshold 锁住默认阈值 60s。
// 若被误改小会误杀长推理回合，改大会回归 5 分钟空转 bug。
func TestTranslateStream_NoContentStall_DefaultThreshold(t *testing.T) {
	if noContentStallTimeout != 60*time.Second {
		t.Fatalf("noContentStallTimeout 默认值必须为 60s，实际 %v\n"+
			"（与 upstreamStallTimeout 同值同语义；< 60s 误杀长推理，> 60s 回归 5m 空转）",
			noContentStallTimeout)
	}
}

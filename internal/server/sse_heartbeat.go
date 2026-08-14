package server

import (
	"context"
	"net/http"
	"time"
)

// @AI_GUARD: SSE_HEARTBEAT_FACTORY - 统一心跳 goroutine 工厂，所有 handler 必须使用此函数
// @CONSTRAINT: 调用方必须遵循 close(callDone) → <-callFinished 清理顺序（顺序不可颠倒）
//   - callDone 关闭 → 心跳 goroutine 退出 → close(callFinished) → 主 goroutine 在 <-callFinished 解除阻塞
//   - 如果先 <-callFinished 再 close(callDone)，会导致死锁
//   - 遗漏 <-callFinished 会导致心跳 goroutine 与主 goroutine 并发写 w → panic / SSE 数据损坏
//
// @RELATED: all handlers with heartbeat, heartbeatEvent, CALLDONE_CALLFINISHED
// @REASON: 历史血泪教训 - 7 处重复的心跳 goroutine 代码（quick.go 5 处 + gateway.go 2 处），
//
//	修改一处遗漏其他导致问题。统一为工厂函数后只需维护一处。
//
// StartSSEHeartbeat 启动 SSE 心跳 goroutine，在等待上游响应期间定期发送心跳事件。
// 返回 callDone 和 callFinished 通道用于控制心跳生命周期。
func StartSSEHeartbeat(w http.ResponseWriter, flusher http.Flusher, ctx context.Context) (callDone chan struct{}, callFinished chan struct{}) {
	callDone = make(chan struct{})
	callFinished = make(chan struct{})
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		defer close(callFinished)
		for {
			select {
			case <-callDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.Write(heartbeatEvent)
				flusher.Flush()
			}
		}
	}()
	return
}
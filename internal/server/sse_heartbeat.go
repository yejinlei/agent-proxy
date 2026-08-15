package server

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"
)

// MutexSSEWriter wraps http.ResponseWriter and http.Flusher with a mutex
// to prevent concurrent writes between heartbeat goroutine and TranslateStream callback.
// Go's http.ResponseWriter is NOT thread-safe — concurrent Write calls corrupt SSE output.
type MutexSSEWriter struct {
	w  http.ResponseWriter
	f  http.Flusher
	mu *sync.Mutex
}

func NewMutexSSEWriter(w http.ResponseWriter, f http.Flusher) *MutexSSEWriter {
	return &MutexSSEWriter{w: w, f: f, mu: &sync.Mutex{}}
}

func (m *MutexSSEWriter) Write(data []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.w.Write(data)
}

func (m *MutexSSEWriter) Header() http.Header {
	return m.w.Header()
}

func (m *MutexSSEWriter) WriteHeader(statusCode int) {
	m.w.WriteHeader(statusCode)
}

func (m *MutexSSEWriter) Flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.f.Flush()
}

// newDummyHeartbeat 返回一个立即可关闭的心跳占位通道，对
//
//	Responses 协议等不需要心跳的场景，避免启用心跳 goroutine 但仍
//	保持 close(callDone) → <-callFinished 的清理契约不变。
//
// 使用：
//
//	callDone, callFinished := newDummyHeartbeat()
//	... // 执行无需心跳的上游调用
//	close(callDone)   // 仍可安全关闭（已关闭的 channel close 会 panic，故本函数不 pre-close）
//	<-callFinished    // 立即返回
func newDummyHeartbeat() (callDone chan struct{}, callFinished chan struct{}) {
	callDone = make(chan struct{})
	callFinished = make(chan struct{})
	// 启动一个 goroutine：要么等待 callDone 被关闭，要么立即返回 callFinished
	go func() {
		<-callDone
		close(callFinished)
	}()
	return
}

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
func StartSSEHeartbeat(w http.ResponseWriter, flusher http.Flusher, ctx context.Context, verboseLevel int) (callDone chan struct{}, callFinished chan struct{}) {
	callDone = make(chan struct{})
	callFinished = make(chan struct{})
	go func() {
		heartbeatStart := time.Now()
		heartbeatCount := 0
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		defer close(callFinished)
		defer func() {
			if verboseLevel >= 2 {
				log.Printf("[heartbeat] stopped → %v (total=%d beats)", time.Since(heartbeatStart), heartbeatCount)
			}
		}()
		if verboseLevel >= 2 {
			log.Printf("[heartbeat] started → interval=500ms")
		}
		for {
			select {
			case <-callDone:
				if verboseLevel >= 2 {
					log.Printf("[heartbeat] callDone received → %v", time.Since(heartbeatStart))
				}
				return
			case <-ctx.Done():
				if verboseLevel >= 2 {
					log.Printf("[heartbeat] ctx.Done received → %v", time.Since(heartbeatStart))
				}
				return
			case <-ticker.C:
				heartbeatCount++
				w.Write(heartbeatEvent)
				flusher.Flush()
				if verboseLevel >= 2 {
					log.Printf("[heartbeat] sent #%d → %v", heartbeatCount, time.Since(heartbeatStart))
				}
			}
		}
	}()
	return
}

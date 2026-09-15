package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestStallTimeoutChan_ClosesOnIdle 验证上游静默时 channel 被关闭。
// 关闭会让下游 `for line := range` 退出，走 TranslateStream 的 channel-close 收尾路径。
func TestStallTimeoutChan_ClosesOnIdle(t *testing.T) {
	src := make(chan json.RawMessage) // 永不发数据，永不关闭
	defer close(src)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wrapped := stallTimeoutChan(src, ctx, 50*time.Millisecond, "test", "test")

	select {
	case _, ok := <-wrapped:
		if ok {
			t.Fatal("expected channel to be closed after idle timeout, got open")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel was not closed within 2s")
	}
}

// TestStallTimeoutChan_PassesThrough 验证正常数据不被吞掉。
func TestStallTimeoutChan_PassesThrough(t *testing.T) {
	src := make(chan json.RawMessage, 1)
	src <- json.RawMessage(`{"x":1}`)
	close(src)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wrapped := stallTimeoutChan(src, ctx, 5*time.Second, "test", "test")

	select {
	case line, ok := <-wrapped:
		if !ok {
			t.Fatal("expected data before close")
		}
		if string(line) != `{"x":1}` {
			t.Fatalf("got %s, want %s", string(line), `{"x":1}`)
		}
		// 上游关闭后，包装层也应关闭
		select {
		case _, ok := <-wrapped:
			if ok {
				t.Fatal("expected channel to close after upstream EOF")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("channel was not closed after upstream EOF")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive data")
	}
}

// TestStallTimeoutChan_CtxCancel 验证 ctx 取消时 channel 立即关闭。
func TestStallTimeoutChan_CtxCancel(t *testing.T) {
	src := make(chan json.RawMessage) // 永不发数据
	defer close(src)
	ctx, cancel := context.WithCancel(context.Background())

	wrapped := stallTimeoutChan(src, ctx, 5*time.Second, "test", "test")
	cancel()

	select {
	case _, ok := <-wrapped:
		if ok {
			t.Fatal("expected channel to be closed after ctx cancel, got open")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel was not closed after ctx cancel")
	}
}

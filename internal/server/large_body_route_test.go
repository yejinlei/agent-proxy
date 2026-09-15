package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTranslationStreamRoute 锁住 LARGE_BODY_SKIP_STREAM_TRANSLATION 的阈值边界。
// 大请求走非流式→SSE，小请求走原生流式；nil 下游体按小请求处理（天然安全）。
func TestTranslationStreamRoute(t *testing.T) {
	cases := []struct {
		name string
		body json.RawMessage
		want bool // true = 原生流式
	}{
		{"nil 下游体", nil, true},
		{"空 body", json.RawMessage(`{}`), true},
		{"小 body", json.RawMessage(strings.Repeat("a", 1024)), true},
		{"恰好等于阈值", json.RawMessage(strings.Repeat("a", largeBodyThreshold)), true},
		{"超阈值 1 字节", json.RawMessage(strings.Repeat("a", largeBodyThreshold+1)), false},
		{"典型大请求", json.RawMessage(strings.Repeat("a", 1<<20)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := translationStreamRoute(tc.body); got != tc.want {
				t.Errorf("translationStreamRoute(len=%d) = %v, want %v", len(tc.body), got, tc.want)
			}
		})
	}
}

// TestLargeBodyThreshold_Value 锁住阈值本身：两处（透传 + 翻译）必须共用同一值。
func TestLargeBodyThreshold_Value(t *testing.T) {
	if largeBodyThreshold != 100*1024 {
		t.Errorf("largeBodyThreshold = %d, want %d", largeBodyThreshold, 100*1024)
	}
}

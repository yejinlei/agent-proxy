package server

// WS 端到端测试 — 通过真实 HTTP 服务器（httptest + Hijacker）验证
// handleResponsesWebSocket 的完整链路：101 握手 → 读帧（含前置 ping）→
// 响应以 WS text 帧回传。覆盖 v0.2.111 修复在集成层面的行为：
//   - 握手后先发 Ping 控制帧，再发 response.create —— 旧实现会把 ping 当请求体
//   - 超大帧被拒绝且连接关闭（无内存放大）
//   - 快速模式（quick.go）与复杂模式（gateway.go）双模式一致
//
// 本文件为新增测试，无生产代码 importer；仅 exercise quick.go/gateway.go 已有的
// Routes()、handleResponses、handleResponsesWebSocket、quickReadWSFrame/readWSFrame。

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/agent-proxy/agent-proxy/internal/config"
)

// wsUpgradeConn 完成一次真实的 WebSocket 升级握手，返回带缓冲读端的连接与帧流。
func wsUpgradeConn(t *testing.T, srvURL, path string) (net.Conn, *bufio.Reader, chan []byte) {
	t.Helper()
	u, err := url.Parse(srvURL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	reqKey := "dGhlIHNhbXBsZSBub25jZQ==" // RFC 6455 §1.3 示例
	req := "GET " + path + " HTTP/1.1\r\nHost: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + reqKey + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		t.Fatalf("write handshake: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		conn.Close()
		t.Fatalf("read handshake response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		t.Fatalf("expected 101, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != quickComputeAcceptKey(reqKey) {
		conn.Close()
		t.Fatalf("Sec-WebSocket-Accept mismatch: %q", got)
	}
	frames := make(chan []byte, 64)
	go func() {
		defer close(frames)
		buf := make([]byte, 65536)
		for {
			n, err := br.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				select {
				case frames <- cp:
				default: // 背压满时丢弃统计意义不大，但避免 goroutine 泄漏阻塞
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return conn, br, frames
}

// wsSend 向连接写一个掩码客户端帧。
func wsSend(t *testing.T, conn net.Conn, opcode byte, payload []byte) {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(wsClientFrame(opcode, payload)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// wsGrabUntil 累积帧流字节直到 want 满足或超时。
func wsGrabUntil(t *testing.T, frames chan []byte, want func([]byte) bool, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.After(timeout)
	var acc []byte
	for {
		if want(acc) {
			return acc
		}
		select {
		case b, ok := <-frames:
			if !ok {
				return acc
			}
			acc = append(acc, b...)
		case <-deadline:
			return acc
		}
	}
}

func runWSEndToEnd(t *testing.T, routes func() chi.Router, mode string) {
	t.Helper()
	srv := httptest.NewServer(routes())
	defer srv.Close()

	t.Run(mode+"/ping-before-request", func(t *testing.T) {
		conn, _, frames := wsUpgradeConn(t, srv.URL, "/v1/responses")
		defer conn.Close()

		wsSend(t, conn, qwsOpPing, []byte("hb"))
		wsSend(t, conn, qwsOpText, []byte(`{"model":"missing-model-test","input":[]}`))

		// 1) 必须收到 pong（证明 ping 未被当作请求体）
		acc := wsGrabUntil(t, frames, func(b []byte) bool { return bytes.Contains(b, []byte{0x8A}) }, 3*time.Second)
		if !bytes.Contains(acc, []byte{0x8A}) {
			t.Fatal("no pong control frame received; ping was likely consumed as request body")
		}
		// 2) pong 之后必须有 text 帧（响应流开始）。上游指向死端口，5s 内必有错误响应或断开；
		//    给 10s 窗口容忍慢拒绝。
		var payload []byte
		deadline := time.After(10 * time.Second)
		for {
			_, payload = wsTryFirstTextAfterPong(acc)
			if len(payload) > 0 {
				break
			}
			select {
			case b, ok := <-frames:
				if !ok {
					t.Fatalf("frame stream closed before a text response arrived (last acc %d bytes)", len(acc))
				}
				acc = append(acc, b...)
			case <-deadline:
				t.Fatalf("timeout waiting for text response frame after ping/pong (acc %d bytes)", len(acc))
			}
		}
	})

	t.Run(mode+"/oversized-frame-closes", func(t *testing.T) {
		conn, br, _ := wsUpgradeConn(t, srv.URL, "/v1/responses")
		defer conn.Close()
		// 只发表头声明 64MB+1，不发载荷 —— 服务端必须在分配前拒绝并断开
		hdr := []byte{0x81, 0x80 | 127}
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(qwsMaxPayload)+1)
		if _, err := conn.Write(append(hdr, ext...)); err != nil {
			t.Fatalf("write oversized header: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 256)
		n, err := br.Read(buf)
		if err == nil && n > 0 {
			t.Fatalf("unexpected data from server after oversized header: %q", buf[:n])
		}
		if err == nil {
			err = io.EOF
		}
		if !(strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "reset") || strings.Contains(err.Error(), "closed")) {
			t.Fatalf("expected connection teardown, got: %v", err)
		}
	})
}

// wsTryFirstTextAfterPong 在原始字节流中定位 pong 帧之后的第一个完整 text 帧。
func wsTryFirstTextAfterPong(raw []byte) (byte, []byte) {
	pos := 0
	seenPong := false
	for pos+2 <= len(raw) {
		opcode := raw[pos] & 0x0F
		l := int(raw[pos+1] & 0x7F)
		hdr := 2
		switch l {
		case 126:
			if pos+4 > len(raw) {
				return 0, nil
			}
			l = int(binary.BigEndian.Uint16(raw[pos+2 : pos+4]))
			hdr = 4
		case 127:
			if pos+10 > len(raw) {
				return 0, nil
			}
			l = int(binary.BigEndian.Uint64(raw[pos+2 : pos+10]))
			hdr = 10
		}
		if pos+hdr+l > len(raw) {
			return 0, nil
		}
		payload := raw[pos+hdr : pos+hdr+l]
		pos += hdr + l
		if opcode == qwsOpPong {
			seenPong = true
			continue
		}
		if seenPong && opcode == qwsOpText {
			return opcode, payload
		}
	}
	return 0, nil
}

func TestQuickModeWS_EndToEnd(t *testing.T) {
	q := NewQuickGateway("t", "http://127.0.0.1:1", "k", []string{"responses"}, nil, "openai", "/v1/chat/completions", 5, "", false, 0)
	runWSEndToEnd(t, q.Routes, "quick")
}

func TestComplexModeWS_EndToEnd(t *testing.T) {
	cfg := &config.Config{}
	cfg.Monitor.LogSize = 100
	g := NewGateway(cfg, 0)
	runWSEndToEnd(t, g.Routes, "gateway")
}

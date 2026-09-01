package server

// WS 帧层单元测试 — 验证 v0.2.111 修复：
//   1. readWSFrame / quickReadWSFrame 自动应答 ping、跳过 pong/close，只把 text 数据帧交给调用方
//      （旧实现把任意首帧当请求体 → Codex 握手后前置 Ping/续接帧会导致 JSON 解析失败 → 降级 HTTP）
//   2. 超大帧（>64MB）被拒绝，不分配内存
//   3. 双模式（quick.go / gateway.go）行为一致
//
// 本文件为新增测试，无生产代码 importer；仅覆盖 quick.go/gateway.go 中已有的
// quickReadWSFrame / readWSFrame / qwsResponseWriter.Write。

import (
	"bytes"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

// wsTestMaskKey 测试用固定掩码 key。
var wsTestMaskKey = [4]byte{0xAA, 0xBB, 0xCC, 0xDD}

// wsClientFrame 构造一个客户端→服务端的掩码帧（RFC 6455）。
func wsClientFrame(opcode byte, payload []byte) []byte {
	var out bytes.Buffer
	out.WriteByte(0x80 | opcode)
	l := len(payload)
	switch {
	case l < 126:
		out.WriteByte(0x80 | byte(l))
	case l < 65536:
		out.WriteByte(0x80 | 126)
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(l))
		out.Write(b[:])
	default:
		out.WriteByte(0x80 | 127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(l))
		out.Write(b[:])
	}
	out.Write(wsTestMaskKey[:])
	masked := make([]byte, l)
	for i := 0; i < l; i++ {
		masked[i] = payload[i] ^ wsTestMaskKey[i%4]
	}
	out.Write(masked)
	return out.Bytes()
}

// wsParseFrame 解析服务端写出的非掩码帧，返回 opcode、payload 及消费字节数。
func wsParseFrame(t *testing.T, raw []byte) (byte, []byte, int) {
	t.Helper()
	if len(raw) < 2 {
		t.Fatalf("frame too short: %d bytes", len(raw))
	}
	opcode := raw[0] & 0x0F
	if raw[1]&0x80 != 0 {
		t.Fatalf("server frame must not be masked")
	}
	l := int(raw[1] & 0x7F)
	hdr := 2
	switch l {
	case 126:
		if len(raw) < 4 {
			t.Fatalf("frame truncated at extended length")
		}
		l = int(binary.BigEndian.Uint16(raw[2:4]))
		hdr = 4
	case 127:
		if len(raw) < 10 {
			t.Fatalf("frame truncated at extended length")
		}
		l = int(binary.BigEndian.Uint64(raw[2:10]))
		hdr = 10
	}
	if len(raw) < hdr+l {
		t.Fatalf("frame truncated: have %d want %d", len(raw), hdr+l)
	}
	return opcode, raw[hdr : hdr+l], hdr + l
}

// wsCollect 从 written channel 收集一段连续字节（等待第一块，再排空已就绪的）。
func wsCollect(t *testing.T, written chan []byte) []byte {
	t.Helper()
	var out []byte
	select {
	case b := <-written:
		out = append(out, b...)
	case <-time.After(time.Second):
		t.Fatal("no frame written")
	}
	for {
		select {
		case b := <-written:
			out = append(out, b...)
		default:
			return out
		}
	}
}

// wsFirstFrame 解析缓冲区中第一个完整帧，返回 opcode 与 payload。
func wsFirstFrame(t *testing.T, raw []byte) (byte, []byte) {
	t.Helper()
	op, payload, _ := wsParseFrame(t, raw)
	return op, payload
}

// wsScriptedConn 提供 scripted 的读端（预置字节流），写出走真实 net.Pipe 另一端。
type wsScriptedConn struct {
	net.Conn
	r *bytes.Reader
}

func (c wsScriptedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// runWSCase 在 net.Pipe 上跑一个用例：peer 收到 frames，read 函数从 srv 侧读取，
// srv 侧写出的帧由 peer 捕获并送入 returned channel。
func runWSCase(t *testing.T, read func(net.Conn) ([]byte, error), frames ...[]byte) ([]byte, error, chan []byte) {
	t.Helper()
	peer, srv := net.Pipe()
	t.Cleanup(func() { peer.Close(); srv.Close() })

	written := make(chan []byte, 16)
	go func() {
		defer close(written)
		buf := make([]byte, 4096)
		for {
			n, err := peer.Read(buf)
			if err != nil {
				return
			}
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				written <- cp
			}
		}
	}()

	conn := wsScriptedConn{Conn: srv, r: bytes.NewReader(bytes.Join(frames, nil))}
	got, err := read(conn)
	return got, err, written
}

func TestQuickReadWSFrame_TextOnly(t *testing.T) {
	body := []byte(`{"type":"response.create","model":"vmodel"}`)
	got, err, _ := runWSCase(t, quickReadWSFrame, wsClientFrame(qwsOpText, body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("payload mismatch: got %q", got)
	}
}

func TestQuickReadWSFrame_PingBeforeRequest(t *testing.T) {
	ping := []byte("ka")
	body := []byte(`{"type":"response.create"}`)
	got, err, written := runWSCase(t, quickReadWSFrame,
		wsClientFrame(qwsOpPing, ping),
		wsClientFrame(qwsOpPong, nil),
		wsClientFrame(qwsOpText, body),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("payload mismatch: got %q", got)
	}
	// 必须回一个 pong，payload 与 ping 一致
	raw := wsCollect(t, written)
	op, payload := wsFirstFrame(t, raw)
	if op != qwsOpPong {
		t.Fatalf("expected pong (0xA), got opcode 0x%X", op)
	}
	if !bytes.Equal(payload, ping) {
		t.Fatalf("pong payload mismatch: got %q want %q", payload, ping)
	}
}

func TestQuickReadWSFrame_CloseHandled(t *testing.T) {
	_, err, written := runWSCase(t, quickReadWSFrame, wsClientFrame(qwsOpClose, []byte{0x03, 0xE8}))
	if err == nil || !strings.Contains(err.Error(), "closed by peer") {
		t.Fatalf("expected closed-by-peer error, got %v", err)
	}
	raw := wsCollect(t, written)
	op, code := wsFirstFrame(t, raw)
	if op != qwsOpClose {
		t.Fatalf("expected close echo, got opcode 0x%X", op)
	}
	if binary.BigEndian.Uint16(code) != 1000 {
		t.Fatalf("expected close code 1000, got %d", binary.BigEndian.Uint16(code))
	}
}

func TestReadWSFrame_GatewayParity(t *testing.T) {
	// 复杂模式必须与快速模式行为一致（双模式同步约定）
	ping := []byte("p")
	body := []byte(`{"type":"response.create"}`)
	got, err, written := runWSCase(t, readWSFrame,
		wsClientFrame(wsOpPing, ping),
		wsClientFrame(wsOpText, body),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("payload mismatch: got %q", got)
	}
	raw := wsCollect(t, written)
	if op, _ := wsFirstFrame(t, raw); op != wsOpPong {
		t.Fatalf("expected pong, got opcode 0x%X", op)
	}
}

func TestReadWSFrame_OversizedRejected(t *testing.T) {
	oversizedHdr := func(cap_ uint64) []byte {
		hdr := []byte{0x81, 0x80 | 127} // FIN|text, masked, len=127
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, cap_)
		return append(hdr, ext...)
	}
	for _, tc := range []struct {
		name string
		read func(net.Conn) ([]byte, error)
		hdr  []byte
	}{
		{"quick", quickReadWSFrame, oversizedHdr(uint64(qwsMaxPayload + 1))},
		{"gateway", readWSFrame, oversizedHdr(uint64(wsMaxPayload + 1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, srv := net.Pipe()
			defer client.Close()
			defer srv.Close()
			go srv.Write(tc.hdr)
			done := make(chan error, 1)
			go func() {
				_, err := tc.read(client)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "too large") {
					t.Fatalf("expected too-large error, got %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("read did not return (likely attempted huge allocation)")
			}
		})
	}
}

func TestQWSResponseWriter_WriteFrames(t *testing.T) {
	client, srv := net.Pipe()
	defer client.Close()
	defer srv.Close()

	w := &qwsResponseWriter{conn: srv}
	payload := []byte("event: response.created\ndata: {}\n\n")
	go func() {
		if _, err := w.Write(payload); err != nil {
			t.Errorf("write failed: %v", err)
		}
	}()

	buf := make([]byte, 4096)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	op, got := wsFirstFrame(t, buf[:n])
	if op != qwsOpText {
		t.Fatalf("expected text frame, got opcode 0x%X", op)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("frame payload mismatch: %q", got)
	}
}

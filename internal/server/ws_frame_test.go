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
	"encoding/json"
	"log"
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

// wsDrain 收集 written channel 里的帧：先用短超时等一次（覆盖 net.Pipe 写出已解
// 阻塞、但捕获 goroutine 还没把字节塞进 channel 的竞态窗口），再非阻塞排空。
// 用于断言"没有多余控制帧"，不会像 wsCollect 那样在无帧时 Fatal。
func wsDrain(written chan []byte) []byte {
	var out []byte
	select {
	case b := <-written:
		out = append(out, b...)
	case <-time.After(100 * time.Millisecond):
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
	// Codex WS 路径按裸 JSON 解析整帧（serde_json::from_str，不剥 SSE 前缀），
	// 帧内容不得带 event:/data: 信封，否则每个事件都解析失败被静默丢弃。
	if !bytes.Equal(got, []byte("{}")) {
		t.Fatalf("frame payload: got %q, want raw JSON %q (SSE envelope must be stripped)", got, "{}")
	}
}

// TestUnwrapWSSEFrame 覆盖 SSE 信封剥离的全部边界。这些边界是 WS 通道的直接故障源：
// WS 与 HTTPS 是两条线协议，同一条事件流在 WS 上是裸 JSON、在 HTTPS 上是 SSE。
func TestUnwrapWSSEFrame(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		want     string
		passthru bool
	}{
		{
			name: "standard single event",
			in:   "event: response.created\ndata: {\"type\":\"response.created\"}\n\n",
			want: `{"type":"response.created"}`,
		},
		{
			// OpenAI 标准写法是 data: 后两个空格（SSE 规范要求 1 个，允许更多），
			// 只按 "data: " 单空格剥前缀会丢一个字符
			name: "double space prefix",
			in:   "event: a\ndata:  {\"a\":1}\n\n",
			want: `{"a":1}`,
		},
		{
			// [DONE] is not valid JSON: forwarding it would make Codex serde_json::from_str
			// return Err, which aborts the whole stream. Rewriting it as a fabricated
			// response.completed would fail the same way - ResponseCompleted.id is a
			// required String, so {"type":"response.completed"} cannot parse either.
			// Dropping is therefore correct, and safe: the real response.completed has
			// already been sent and already breaks the event loop.
			name:     "done marker",
			in:       "event: done\ndata: [DONE]\n\n",
			want:     "event: done\ndata: [DONE]\n\n",
			passthru: true,
		},
		{
			// When [DONE] is dropped but a real event remains, we must NOT fall back to
			// the raw envelope - that would put the event back on WS still wrapped, i.e.
			// the exact pre-fix failure mode.
			name: "done marker followed by real event",
			in:   "data: [DONE]\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
			want: `{"type":"response.completed"}`,
		},
		{
			// 多事件批：event: 名行不是事件体必须过滤，空 data: 行只是分隔符
			name: "multi event batch",
			in:   "event: a\ndata: {\"t\":1}\ndata: \nevent: b\ndata: {\"t\":2}\n\n",
			want: `{"t":1}` + "\n" + `{"t":2}`,
		},
		{
			name:     "non-sse passthrough",
			in:       "plain text without sse",
			want:     "plain text without sse",
			passthru: true,
		},
		{
			name:     "empty input",
			in:       "",
			want:     "",
			passthru: true,
		},
		{
			name: "crlf line endings",
			in:   "event: a\r\ndata: {\"c\":1}\r\n\r\n",
			want: `{"c":1}`,
		},
		{
			name:     "event line only no data",
			in:       "event: response.created\n\n",
			want:     "event: response.created\n\n",
			passthru: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unwrapWSSEFrame([]byte(tc.in))
			if string(got) != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
			if !tc.passthru {
				if bytes.Contains(got, []byte("event:")) || bytes.Contains(got, []byte("data:")) {
					t.Errorf("SSE envelope residue in %q", got)
				}
				// Output is "one event per frame", so verify each line is standalone JSON
				// (Codex does one serde_json::from_str per frame; a non-JSON line drops the frame)
				for _, line := range bytes.Split(got, []byte("\n")) {
					if len(line) == 0 {
						continue
					}
					var probe map[string]interface{}
					if json.Unmarshal(line, &probe) != nil {
						t.Errorf("frame %q is not parseable JSON", line)
					}
				}
			}
		})
	}
}

// TestBothWSWriters_StripEnvelope 双模式同步：quick 与 gateway 的 WS writer 都必须剥信封，
// 否则「修 A 坏 B」——这正是 quick.go/gateway.go 双模式同步规则的用途。
func TestBothWSWriters_StripEnvelope(t *testing.T) {
	in := []byte("event: response.output_text.delta\ndata: {\"delta\":\"hi\"}\n\n")
	want := `{"delta":"hi"}`

	for _, tc := range []struct {
		name string
		do   func() error
	}{
		{
			name: "quick",
			do: func() error {
				client, srv := net.Pipe()
				defer client.Close()
				defer srv.Close()
				w := &qwsResponseWriter{conn: srv}
				go func() { _, _ = w.Write(in) }()
				buf := make([]byte, 4096)
				n, err := client.Read(buf)
				if err != nil {
					return err
				}
				if _, got := wsFirstFrame(t, buf[:n]); string(got) != want {
					t.Errorf("quick: got %q, want %q", got, want)
				}
				return nil
			},
		},
		{
			name: "gateway",
			do: func() error {
				client, srv := net.Pipe()
				defer client.Close()
				defer srv.Close()
				w := &wsResponseWriter{conn: srv}
				go func() { _, _ = w.Write(in) }()
				buf := make([]byte, 4096)
				n, err := client.Read(buf)
				if err != nil {
					return err
				}
				if _, got := wsFirstFrame(t, buf[:n]); string(got) != want {
					t.Errorf("gateway: got %q, want %q", got, want)
				}
				return nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = tc.do()
		})
	}
}

func TestWSWriteFailureLogged(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	for _, tc := range []struct {
		name  string
		write func() error
	}{
		{"quick", func() error {
			peer, srv := net.Pipe()
			defer peer.Close()
			defer srv.Close()
			peer.Close() // 对端先关，srv 侧写出必然失败
			w := &qwsResponseWriter{conn: srv}
			_, err := w.Write([]byte("event: response.created\ndata: {}\n\n"))
			return err
		}},
		{"gateway", func() error {
			peer, srv := net.Pipe()
			defer peer.Close()
			defer srv.Close()
			peer.Close()
			w := &wsResponseWriter{conn: srv}
			_, err := w.Write([]byte("event: response.created\ndata: {}\n\n"))
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()
			if err := tc.write(); err == nil {
				t.Fatal("expected write error against closed peer")
			}
			out := buf.String()
			for _, frag := range []string{"WS frame → client FAILED", "err=", "preview="} {
				if !strings.Contains(out, frag) {
					t.Errorf("FAILED 日志缺片段 %q，实际: %q", frag, out)
				}
			}
			if strings.Contains(out, "\n") && !strings.HasSuffix(out, "\n") {
				t.Errorf("FAILED 日志不是单行（会被打散无法归属）: %q", out)
			}
			t.Logf("log: %s", out)
		})
	}
}

// TestWSPreview 双模式共用的帧头预览：截断到 120 字节并压成单行。
func TestWSPreview(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"短帧原样", "event: ping", "event: ping"},
		{"结尾换行去除", "event: done\ndata: [DONE]\n\n", "event: done data: [DONE]"},
		{"内部换行压平", "line1\nline2", "line1 line2"},
		{"回车压平", "a\r\nb\r", "a b"},
		{"超长截断到120", strings.Repeat("x", 300), strings.Repeat("x", 120)},
		{"空帧", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wsPreview([]byte(tc.in)); got != tc.want {
				t.Errorf("wsPreview(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// wsFrameAt builds one masked client frame carrying wcBody[off:off+n].
// It reuses wsClientFrame so the length encoding stays correct at every
// payload size - the previous hand-rolled 126-length form could only express
// 0..255 bytes and silently zeroed the high length byte. wsClientFrame always
// sets FIN, so clear it explicitly when the frame must stay open.
func wsFrameAt(fin bool, opcode byte, off, n int) []byte {
	if n <= 0 {
		n = len(wcBody) - off
	}
	if n > len(wcBody)-off {
		n = len(wcBody) - off
	}
	frame := wsClientFrame(opcode, wcBody[off:off+n])
	if fin {
		frame[0] |= 0x80
	} else {
		frame[0] &= 0x7F
	}
	return frame
}

// wcBody is the message the fragmented cases reassemble. It is deliberately a
// valid request so a correct reassembly would be usable end to end.
var wcBody = []byte(`{"model":"vmodel","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ok"}]}],"max_output_tokens":30}`)

// TestQuickReadWSFrame_FragmentedMessage covers RFC 6455 section 5.4: a text
// message may arrive as an initial frame plus continuation frames. Before the
// fix those frames were skipped by the opcode switch, so the reader returned a
// truncated message that failed JSON parse on the codex side and forced an
// HTTPS fallback. The reader must return the whole reassembled message.
func TestQuickReadWSFrame_FragmentedMessage(t *testing.T) {
	t.Run("two fragments", func(t *testing.T) {
		mid := len(wcBody) / 2
		f1 := wsFrameAt(false, qwsOpText, 0, mid)
		f2 := wsFrameAt(true, 0x0, mid, len(wcBody)-mid)
		got, err, written := runWSCase(t, quickReadWSFrame, f1, f2)
		if err != nil {
			t.Fatalf("read failed: %v", err)
		}
		if string(got) != string(wcBody) {
			t.Fatalf("message truncated: got %d bytes, want %d", len(got), len(wcBody))
		}
		if len(wsDrain(written)) != 0 {
			t.Errorf("no control frame expected, got one")
		}
	})

	t.Run("three fragments", func(t *testing.T) {
		t1 := len(wcBody) / 3
		t2 := t1 * 2
		frames := [][]byte{
			wsFrameAt(false, qwsOpText, 0, t1),
			wsFrameAt(false, 0x0, t1, t2-t1),
			wsFrameAt(true, 0x0, t2, len(wcBody)-t2),
		}
		got, err, _ := runWSCase(t, quickReadWSFrame, frames...)
		if err != nil {
			t.Fatalf("read failed: %v", err)
		}
		if string(got) != string(wcBody) {
			t.Fatalf("message truncated: got %d bytes, want %d", len(got), len(wcBody))
		}
	})

	t.Run("ping interleaved mid fragment", func(t *testing.T) {
		// RFC 6455 section 5.5 lets a control frame split a fragmented message.
		// The reader must answer the ping and stay in sync, not fail the request.
		mid := len(wcBody) / 2
		frames := [][]byte{
			wsFrameAt(false, qwsOpText, 0, mid),
			wsClientFrame(qwsOpPing, []byte("keepalive")),
			wsFrameAt(true, 0x0, mid, len(wcBody)-mid),
		}
		got, err, written := runWSCase(t, quickReadWSFrame, frames...)
		if err != nil {
			t.Fatalf("read failed: %v", err)
		}
		if string(got) != string(wcBody) {
			t.Fatalf("message truncated: got %d bytes, want %d", len(got), len(wcBody))
		}
		// 必须回一个 pong 并回显 payload，且不能多写帧
		extra := wsDrain(written)
		if len(extra) == 0 {
			t.Fatal("expected a pong frame, got none")
		}
		op, payload := wsFirstFrame(t, extra)
		if op != qwsOpPong {
			t.Fatalf("opcode = 0x%X, want 0xA (pong)", op)
		}
		if string(payload) != "keepalive" {
			t.Fatalf("pong payload = %q, must echo the ping payload", payload)
		}
	})
}

// TestReadWSFrame_FragmentedMessage_Gateway pins the complex-mode reader to the
// same reassembly behaviour - quick.go and gateway.go must not drift here.
func TestReadWSFrame_FragmentedMessage_Gateway(t *testing.T) {
	mid := len(wcBody) / 2
	frames := [][]byte{
		wsFrameAt(false, wsOpText, 0, mid),
		wsClientFrame(wsOpPing, []byte("keepalive")),
		wsFrameAt(true, 0x0, mid, len(wcBody)-mid),
	}
	got, err, written := runWSCase(t, readWSFrame, frames...)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(got) != string(wcBody) {
		t.Fatalf("message truncated: got %d bytes, want %d", len(got), len(wcBody))
	}
	raw := wsCollect(t, written)
	op, payload := wsFirstFrame(t, raw)
	if op != wsOpPong {
		t.Fatalf("expected pong, got opcode 0x%X", op)
	}
	if string(payload) != "keepalive" {
		t.Fatalf("pong payload = %q, must echo the ping payload", payload)
	}
}

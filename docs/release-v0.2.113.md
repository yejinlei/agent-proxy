# v0.2.113

## 修复

### WS 心跳格式错误导致 Codex 客户端 keepalive 不重置（broken pipe）

**问题：** v0.2.112 修复了 WS 写错误的静默吞掉后，Codex 侧单轮"你好"仍花数分钟并 fallback 到 HTTPS。代理日志显示同一 Codex 会话里 8 次 WS 握手全部以 `broken pipe` 退出（17:28–17:31 期间），服务端最终 fallback 到 HTTPS transport 才拿到答复。

**日志取证（`agent-proxy-9091.log`）：**

| 时间 | 连接 | 间隔 | handler 耗时 | 结局 |
|------|------|------|-------------|------|
| 17:28:37 | 49831 | — | 17s | ❌ broken pipe |
| 17:28:51 | 49838 | — | 34s | ❌ broken pipe |
| 17:28:51 | 49839 | — | — | ❌ broken pipe |
| 17:28:59 | 49847 | — | 7.6s | ❌ broken pipe |
| 17:29:07 | 49853 | — | 18s | ❌ broken pipe |
| 17:29:35 | 49875 | — | 7s | ❌ broken pipe |
| 17:29:42 | 49885 | — | 13s | ❌ broken pipe |
| 17:29:55 | 49905 | — | 12s | ❌ broken pipe |

**Root cause：** WS 心跳用 `quickWriteWSCtrl(qwsOpPing)` 发的是 **RFC 6455 ping 控制帧（opcode 0x9）**。RFC 6455 规定 ping 控制帧在 WS 栈内**透明处理**，不上传到应用层——客户端收到后自动回 pong，**应用层不可见**，不会被计入"内容活动"，**keepalive 定时器不重置**。SenseNova 冷启动 10–35s 的静默期里，客户端判定通道停滞 → 主动关 TCP → 服务端写帧 `broken pipe`。

**对照证据：** SSE 通道用 `heartbeatEvent`（`event: ping\ndata: {"type":"ping"}`）时同格式心跳正常工作，且 v0.2.113 最终那条 HTTPS fallback 也正常工作——说明 Anthropic 兼容的应用层 ping 事件格式是有效的，只是 WS 层没用它。

**修复（quick.go + gateway.go 双模式同步）：**

`WritePing` 从发 ping 控制帧改为发 **text 帧（opcode 0x1）承载 `heartbeatEvent`**：
- `quick.go:3764` `quickWriteWSCtrl(w.conn, qwsOpPing, nil)` → `quickWriteWSFrame(w.conn, heartbeatEvent)`
- `gateway.go:1754` `writeWSCtrl(w.conn, wsOpPing, nil)` → `writeWSFrame(w.conn, heartbeatEvent)`
- 同步把 WS 心跳频率从 15s 缩到 5s（quick.go:3852 + gateway.go:1840）
- 新增 `@AI_GUARD: WS_HEARTBEAT_FORMAT` 标记，固化"WS 心跳必须用 text 帧承载应用层 ping，不可用 ping 控制帧"

这样客户端就能在应用层收到 `event: ping`，keepalive 定时器正确重置，SenseNova 冷启动期间的静默不再触发断连。

**未改动：** Anthropic 协议的 `TranslateStream` 等翻译器逻辑未动，符合"涉及claude协议先不动"的约定——本修复只触及 WS 通道层。

**验证：** `go build ./...`、`go test -count=1 ./internal/server/` 通过（23.316s）。

| 文件 | 改动 |
|------|------|
| `internal/server/quick.go` | `WritePing` 改发 text 帧承载 `heartbeatEvent` + WS 心跳 15s→5s |
| `internal/server/gateway.go` | 同上镜像 |
| `main.go` | 版本号 `v0.2.112` → `v0.2.113` |

**诚实边界：** 5s 是经验值，Codex 官方 WS keepalive 窗口未公开，5s 留有 ~5s 安全余量。部署后观察 `agent-proxy-9091.log` 是否还有 `WS-ERR broken pipe`。若仍有，备选方案 B（handler 入口立即回 `response.created` 事件）已记录在 memory。
# v0.2.111

## 修复

### 1. Codex WebSocket 通道加固（修正 v0.2.110 的「无需修复」结论）

**问题：** Codex CLI（v0.151.0）经 agent-proxy 接入时，会话初期走 `ws://` WebSocket 通道，若干轮之后自动降级为 HTTP POST。v0.2.110 发布说明曾判定「降级发生在客户端侧，代理零责任」——**该结论错误**，本版本修正。

**重新分析（日志 + 代码审计）：** 对 `agent-proxy-9091.log` 的逐帧取证表明：

- 14 次 WS 握手全部 101 成功；WS 时代（日志 124–1440 行）确实完整交付了 13 轮纯文本响应（96 个帧）——传输层本身工作正常；
- 但首个携带 14 个工具定义的 WS 回合（conn 5173，46KB 请求帧，19:53:28）**零帧应答、永不完成**，Codex 只能在新的 WS 连接上重发相同请求体（conn 5179、5183…）；
- 整个 WS 时代**从未出现过任何** `response.function_call_arguments.*` 事件（9 次工具调用事件全部在降级后的 HTTP 时代）；
- 最终降级点（19:54:28）恰好发生在一轮成功的 tools=0 WS 交换之后 —— 与「通道被判定停滞/不可靠后回落」的模式吻合。

结合代码审计，代理侧 WS 实现存在 **5 个真实缺陷**，任何一个都足以让 Codex/tungstenite 判定通道不可靠：

| # | 缺陷 | 后果 |
|---|------|------|
| 1 | **控制帧盲读**：`readWSFrame` 把握手后收到的第一个帧无条件当请求体 JSON 解析。Codex/tungstenite 可能先发 Ping keepalive 或续接帧 | JSON 解析失败 → 空输入 → 本轮无响应 → 判通道坏 |
| 2 | **无 WS 层心跳**：Responses 流式路径只在「静默 >500ms」时发 SSE ping；短内容快速回答整轮零心跳 | Codex keepalive（~10s）误判通道停滞 → 降级 HTTP |
| 3 | **无写锁**：心跳 goroutine 与 handler goroutine 可对同一 conn 交错裸写 | 帧字节撕裂 → 客户端协议错误 → 断连 |
| 4 | **无写死线**：客户端停滞（TCP 缓冲满）时 handler 永久阻塞 | 连接与上游流泄漏（对应 conn 5173 卡死形态） |
| 5 | **无载荷上限**：帧长度字段直接驱动内存分配 | 恶意/异常超大帧 → OOM 放大 |

**修复（双模式同步：quick.go + gateway.go）：**

- `quickReadWSFrame` / `readWSFrame` 重写为控制帧感知的读循环：text 帧返回载荷；**ping 原样回 pong**（RFC 6455）；pong 忽略；close 回 1000 正常关闭码后断开；未知操作码跳过。新增 `@AI_GUARD: WS_FRAME_CONTROL_FRAMES`。
- 新增 `quickWriteWSCtrl` / `writeWSCtrl`：非掩码控制帧写出助手。
- 单帧载荷上限 64MB（`qwsMaxPayload` / `wsMaxPayload`），超限拒绝且不分配。
- `qwsResponseWriter` / `wsResponseWriter` 增加 `sync.Mutex` 序列化所有写出 + 每帧 60s `SetWriteDeadline`；新增 `WritePing()`。新增 `@AI_GUARD: WS_FLUSHER_NOT_BUFFERED`（Flush 保持 no-op 的理由固化）。
- `handleResponsesWebSocket` 包裹一个 **15s 周期的 WS ping 心跳 goroutine**，handler 结束后取消并等待退出。新增 `@AI_GUARD: WS_HEARTBEAT`。WS 层 ping 控制帧与 SSE 层 `event: ping` 事件正交：前者维持传输层活跃判定（客户端自动回 pong，不进入事件流解析），后者维持入站协议兼容 —— 两者互不替代。

| 文件 | 改动 |
|------|------|
| `internal/server/quick.go` | WS 常量扩展、`quickReadWSFrame` 重写、`quickWriteWSCtrl` 新增、`qwsResponseWriter` 加锁+死线+`WritePing`、心跳 goroutine |
| `internal/server/gateway.go` | 同上镜像实现（`readWSFrame`/`writeWSCtrl`/`wsResponseWriter`），补 `"sync"` 导入 |
| `internal/server/ws_frame_test.go` | 新增：帧层单测 ×6（text-only / ping 前置应答 / close 处理 / 双模式一致性 / 超大帧拒绝 ×2 / 写出帧格式） |
| `internal/server/ws_e2e_test.go` | 新增：端到端测试 ×2 模式（真实 httptest + Hijacker 链路：握手→前置 ping→pong→text 响应；超大帧断开） |
| `main.go` | 版本号 `v0.2.110` → `v0.2.111` |

**验证：** `go build ./...`、`go vet` 通过；`go test ./internal/...` 全部通过（含新增 10 个 WS 测试用例，快速/复杂双模式行为一致已断言）。

**诚实边界：** v0.2.110 之前 Responses 翻译器的工具调用流缺陷（幽灵 fc 条目、缺 per-fc done）同样会污染任何走 WS 的工具调用流；两个版本叠加才给 WS 通道持久化最佳机会。本版本属纵深防御加固 —— 降级是否彻底消失需以实际部署观察为准（升级二进制重启后，用 Codex 连续多轮含工具调用的会话验证 WS 不再回落 HTTP）。

### 勘误

v0.2.110 发布说明附录「### 2. 附带说明：Codex WebSocket 降级为 HTTPS（无需修复）」的结论作废，以本文档为准。

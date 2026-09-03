# v0.2.114

## 修复

### Codex WS 通道 SenseNova 冷启动期间 broken pipe（响应首字节延迟 2–4 分钟）

**问题：** v0.2.113 把 WS 心跳从 ping 控制帧改为 text 帧（5s 周期）后，Codex 侧"你好"仍然耗时数分钟并 fallback 到 HTTPS。`agent-proxy-9091.log` 显示 4 个 WS 连接 handler 实际运行 2–3.5 分钟才退出，全程 0 个 `event: ping` 帧到达客户端，连接被 Codex 提前关闭（broken pipe）。

**Root cause：** `responses/translator.go` `TranslateStream` 的 `sendCreated()` 是 lazy 触发 —— 仅在事件循环的 `case "start"` / `case "done"` 分支调用，而这些分支阻塞等待上游（SenseNova）产出的首个 SSE 事件。SenseNova 冷启动 2–4 分钟不产出任何事件 → `sendCreated()` 永不触发 → Codex 在握手完成后长达数分钟收不到任何应用层字节（`response.created` 是 Responses API 的响应进度信号，仅靠 `event: ping` 心跳不足以重置其 keepalive）。Codex 判定通道停滞 → 主动关 TCP → handler 最终写出时 broken pipe。

对照 Anthropic 协议：其翻译器在 `TranslateStream` 入口即发送 `message_start`（不依赖上游首个事件），是 Responses 翻译器应该遵循的范式，此前遗漏。

**修复（仅改 `internal/protocol/responses/translator.go` 一处）：**

在 `TranslateStream` 的事件 `select` 循环之前立即调用 `sendCreated()`，让 `response.created` + `output_item.added` 在握手完成后毫秒级发出，Codex 立即收到响应进度信号、keepalive 重置、冷启动静默期被容忍。

- `createdSent = true` 状态守卫（既有逻辑，第 552 行）保证后续 `case "start"/"done"` 的 lazy 调用成为 no-op，不会重复发出 `response.created`
- `messageID` 是固定变量，`output_item.added`（eager）与 `output_item.done`（`closeMessageItem`）携带相同 `id`，added/done 对仍然配对正确
- 对全部退出路径（channel 关闭 → `sendCompleted`，error → `finalizeItems`，done → `sendCompleted`）均无影响

| 文件 | 改动 |
|------|------|
| `internal/protocol/responses/translator.go` | `TranslateStream` 入口（select 循环前）新增 `sendCreated()` eager 调用 + `@AI_GUARD: RESPONSES_CREATED_EAGER` 标记 |
| `main.go` | 版本号 `v0.2.113` → `v0.2.114` |

**未改动：** Anthropic 协议翻译器（`anthropic/translator.go`）未动，符合"涉及 claude 协议的先不要动"的约定；`quick.go` / `gateway.go` 的 WS 心跳辅修逻辑保持不变。

**验证：** `go build ./...` 通过、`go test -count=1 ./internal/server/` 通过（19.971s）。

**诚实边界：** 这是 Plan B（TranslateStream 入口立即回 `response.created`）。Plan A（5s text 帧心跳）保留为辅助层；两者叠加后 Codex 应保持 WS 通道活跃。部署后观察 `agent-proxy-9091.log` 是否还有 `WS-ERR broken pipe`、单轮"你好"是否在秒级完成。若仍有（极端冷启动 >keepalive），备选方案 C（Responses 干脆不走 WS，让 Codex 只走 HTTPS）保留在 memory。
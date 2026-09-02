# v0.2.112

## 修复

### WS 通道 TranslateStream 写错误静默吞掉（连接 53875 类故障）

**问题：** v0.2.111 加固了 WS 层（控制帧感知、写锁、写死线、64MB 载荷上限、15s ping 心跳），但 Codex 在携带 13 个工具定义的回合里仍然"Working..."卡住、0 字节输出。

**重新取证（`agent-proxy-9091.log`，2026-09-02 10:51–10:55）：**

| 连接 | 时间 | 载荷 | 工具 | emit | WS frame | 耗时 | 状态 |
|------|------|------|------|------|----------|------|------|
| 53717 | 10:51:42 | 42KB | 13 | 7 | 7 | 75.9s | ✅ |
| 53725 | 10:51:56 | 73KB | 13 | 0 | 0 | — | ⚠️ 无 emit |
| 53726 | 10:51:57 | 19KB | 0 | 0 | 0 | — | ⚠️ 无 emit |
| 53738 | 10:52:12 | 22KB | 0 | 0 | 0 | — | ⚠️ 无 emit |
| 53801 | 10:53:28 | 22KB | 0 | 8 | 8 | 160.8s | ✅ |
| 53851 | 10:54:38 | 73KB | 13 | 7 | 7 | 82.2s | ✅ |
| 53864 | 10:54:51 | 22KB | 0 | 0 | 0 | — | ⚠️ 无 emit |
| **53875** | **10:55:04** | **74KB** | **13** | **11** | **0** | **205.8s** | **❌ 故障** |

**root cause：** 53875 的 11 条 TranslateStream emit 事件完整走完生命周期（含 `response.completed` + `[DONE]`），handler 以 status=200 正常退出，但 `WS frame → client` 日志为 **0 条**。

逐帧比对 8 个连接后发现，故障点不是上游慢、不是 WS 层控制帧盲读、不是载荷超限——是 `qwsResponseWriter.Write` 在 `TranslateStream` 回调里返回了错误（`SetWriteDeadline` 超时或 `broken pipe`），但 callback 静默丢弃了该错误，继续往下 emit 后续事件。handler 走完整个生命周期，Codex 侧 0 字节，卡 Working。

**Codex 工具调用维度：** 53875 的 emit 序列中包含 `response.function_call_arguments.delta` + `response.function_call_arguments.done`（见日志 509、512 行）——这是工具参数事件。Codex 没收到这些事件 → 看不到工具调用 → 不执行工具 → 不喂回工具结果 → turn 永远卡住。v0.2.111 加固的是"能送达"的问题，这个修复解决的是"送达失败后不处理"的问题。

**修复（双模式同步：quick.go + gateway.go）：**

- `TranslateStream` 回调（quick.go `handleStreamRequest` / gateway.go 同名函数）检查 `mw.Write` 错误，命中即 `log.Printf("[WS-ERR] ...")` + `cancel()`（取消 `callCtx`）+ `return`，立即终止。
- 同时把 `CallStream` 也绑到可取消的 `streamCtx`（gateway.go），使 cancel 同时切断上游 HTTP 流和下游 TranslateStream，避免 handler 空转 200s。
- 新增 `@AI_GUARD: STREAM_WRITE_ERROR` 标记，固化 callback 必须检查写错误这一硬约束。

**验证：** `go build ./...`、`go test ./internal/...` 全部通过。

| 文件 | 改动 |
|------|------|
| `internal/server/quick.go` | handleStreamRequest 回调加 Write 错误检查 + cancel + @AI_GUARD |
| `internal/server/gateway.go` | 同名函数镜像修复 + CallStream 绑到 streamCtx |
| `main.go` | 版本号 `v0.2.111` → `v0.2.112` |

**诚实边界：** 服务端修复后，handler 在首个写失败时立即退出，不再等待 200s；但 Codex 侧对"连接意外关闭"的恢复机制（是否自动重试、是否保留该 turn 的工具调用上下文）未实测验证。部署后需观察 Codex 在 WS 通道上能否在首次 write 超时后自行重发该 turn。
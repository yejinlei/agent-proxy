## agent-proxy v0.2.61 — 心跳格式根因修复

### 🔥 核心修复：SSE 心跳格式

**根因**：心跳格式 `: heartbeat\n\n`（SSE 注释）不被 Claude Code 识别为"内容活动"，长上游响应（2m14s）期间 Claude Code 的 HTTP 客户端超时断开，响应数据写入到已断开的连接（数据小到能放入 TCP 缓冲区，`w.Write` 不报错），客户端只收到心跳注释、没有实际 SSE 事件 → "empty or malformed response"。

**修复**：心跳格式从 `: heartbeat\n\n` 改为 `data: {"type":"ping"}\n\n`（Anthropic 标准 ping 事件）。

Claude Code 的 SSE 解析器将 `data:` 行识别为"内容活动"来重置 HTTP 超时计时器，而 SSE 注释（`:` 开头）不会触发此机制。

### 心跳格式演进历史

| 格式 | 问题 |
|------|------|
| `data: {}\n\n` | Claude Code 解析为 Anthropic 事件，缺少 type 字段 → 解析失败 |
| `data: \n\n` | Kimi 等严格客户端空 data 行 JSON.parse 报错 |
| `: heartbeat\n\n` | SSE 注释，Claude Code 不识别为内容活动，不重置 HTTP 超时 |
| **`data: {"type":"ping"}\n\n`** ✅ | Anthropic 标准 ping 事件，Claude Code 识别为内容活动，有效 JSON 不破坏 Kimi |

### 修改文件

| 文件 | 变更 |
|------|------|
| `internal/server/quick.go` | `heartbeatEvent` 改为 `data: {"type":"ping"}`，更新 `@AI_GUARD` 标记 |
| `internal/server/gateway.go` | `@AI_GUARD` 注释同步更新 |
| `internal/web/server.go` | Web UI SSE 心跳同步改为 `data: {"type":"ping"}` |
| `internal/server/e2e_test.go` | 测试断言改为验证 `"type":"ping"`，移除 goroutine 调度竞争的日志时序断言 |
| `AGENTS.md` / `CLAUDE.md` | 心跳格式约定更新，`SSE_HEARTBEAT_FORMAT` 标记描述更新 |

### 📥 下载

| 文件 | 平台 | 架构 |
|------|------|------|
| agent-proxy-windows-amd64.exe | Windows | amd64 |
| agent-proxy-linux-amd64 | Linux | amd64 |
| agent-proxy-darwin-amd64 | macOS | Intel |
| agent-proxy-darwin-arm64 | macOS | Apple Silicon |

### 🔑 SHA256 校验

```
f6095af1609732d24f75a7834f4b6d3a49c98759dfaaae279e5bb165a97054b9  agent-proxy-windows-amd64.exe
09ee4acd12bd9ea6db5c21c178c8c53d6b50751cbb31ef45889cc4eebe68eb2b  agent-proxy-linux-amd64
e6ccb5dd23adf985c741150829bf7a605c29c1900f9cf9d2ef81b20b9cab595a  agent-proxy-darwin-amd64
847c46049e78ddbb1483391ad0cef9a3b9af5c5735a37f76cc3c4269dd57f0e6  agent-proxy-darwin-arm64
```

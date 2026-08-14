## agent-proxy v0.2.60 — 心跳时序根因修复 + 自适应路由简化

### 🔥 核心修复：SSE 心跳时序问题

**根因**：在长延迟上游响应（70s+）场景下，客户端因收不到 SSE 心跳而超时断开。

#### 两阶段心跳保护（`handleStreamRequest`）

`CallStream` 返回 channel 很快（~1ms），但实际数据到达可能需要数秒。此前只有单阶段心跳保护 `CallStream` 调用本身，channel 等待数据的空窗期无任何心跳。

```
修复前（单阶段）：
  [upstream] CallStream → 1ms     ← 心跳立即停止
  [handler] → 5s                 ← 无心跳！客户端超时

修复后（两阶段）：
  阶段 1: 心跳保护 CallStream（HTTP 连接建立）
  阶段 2: 心跳保护流处理（等待上游数据到达）
  [heartbeat] sent #1..#10 覆盖 5s 延迟 ✅
```

#### 心跳在 Call 之前启动（`handlePassthroughNonStreamAsSSE` / `handleNonStreamResponseAsSSE`）

此前心跳在 `Call` 返回后才启动，71s 上游调用期间零心跳。现改为 `WriteHeader(200)` → 心跳 → `Call` → 停心跳 → 写响应。

### 🧹 路由逻辑简化

- 移除 `--stream-mode` CLI 参数（4 种模式过于复杂）
- 统一为基于请求体 `stream` 字段的自适应路由
- 透传路径：`stream=true` → 流式直连，`stream=false` → SSE 包装
- 翻译路径：`stream=true` → `handleStreamRequest`，`stream=false` → `handleNonStreamResponse`

### 📊 `-vv` 详细日志体系

新增多级耗时统计日志（需 `--vv` 启用）：

| 日志前缀 | 含义 |
|---------|------|
| `[request] total` | 请求总耗时 |
| `[handler]` | 各 Handler 耗时 |
| `[upstream] Call/CallStream` | 上游 HTTP 调用耗时 |
| `[heartbeat] started/sent #N/stopped` | 心跳生命周期追踪 |
| `[route]` | 路由决策详情 |

### 🛡️ 双模式同步（quick.go ↔ gateway.go）

所有修复均同步应用到快速模式和复杂模式：
- `handleStreamRequest` 两阶段心跳
- `handlePassthroughNonStreamAsSSE` 心跳时序
- `handleNonStreamResponseAsSSE` 心跳时序
- `-vv` 日志机制

### ✅ 新增测试

`TestHeartbeatDuringLongUpstreamCall` — 模拟 5 秒上游延迟，验证：
- 心跳在 `Call` 之前启动
- 心跳在流处理期间持续发送（>0 beats）
- SSE 响应包含心跳行
- 日志时序正确

### 📥 下载

| 文件 | 平台 | 架构 | 大小 |
|------|------|------|------|
| agent-proxy-windows-amd64.exe | Windows | amd64 | 13.3 MB |
| agent-proxy-linux-amd64 | Linux | amd64 | 12.9 MB |
| agent-proxy-darwin-amd64 | macOS | amd64 | 13.1 MB |
| agent-proxy-darwin-arm64 | macOS | arm64 | 12.6 MB |

### 🔑 SHA256 校验

```
52f75c630b45f3c6d2e749e25c1d4dddebf8513add9b0f1a6db75cb965501247  agent-proxy-windows-amd64.exe
57c73448ef4e7f43c520a7a23b7a942b9eb482e752cbb263a04db02a7f1118ad  agent-proxy-linux-amd64
52ae2ac0e22065906f6f6cf5c41a3f44c761ebfb14863234e352bc2211765e64  agent-proxy-darwin-amd64
7c9c4924b378d55a5eee329417757ff183f3c19e3e62554102522e2c6b181aa0  agent-proxy-darwin-arm64
```

### 💻 快速开始

```powershell
# 下载后运行
./agent-proxy-windows-amd64.exe run --db 1 --vv

# 查看版本
./agent-proxy-windows-amd64.exe version
```
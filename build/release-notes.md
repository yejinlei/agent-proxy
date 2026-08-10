## v0.2.34

### 修复
- **cogitation ECONNRESET 根因修复**：心跳格式从 `event: ping\ndata: \n\n` 改为 `data: \n\n`（空 data 行）。Claude Code 的 SSE 解析器会忽略 `event: ping` 事件，但会将 `data:` 行识别为"内容活动"来重置客户端超时计时器。这是 v0.2.33 仍 ECONNRESET 的真正根因——`event: ping` 不会触发 Claude Code 的超时重置逻辑。
- 心跳间隔从 1s 缩短到 500ms，进一步降低上游 cogitation 期间客户端断开的概率
- http.Server ReadTimeout 从 30s 增加到 120s（配合 313KB 大请求体 + 长时间 cogitation）
- http.Server WriteTimeout 从 120s 增加到 600s，匹配更长请求生命周期

### 根因分析（v0.2.33 为何未修复）
- v0.2.33 将 provider http.Client 超时扩展到 300s、心跳缩短到 1s，但日志仍显示 `context canceled`（不是 `deadline exceeded`）
- `context canceled` = Claude Code 的 HTTP 客户端主动断开连接，取消了请求上下文（`r.Context()`）
- Claude Code 客户端存在 HTTP 层超时（约 15-22s），在无任何 HTTP body 数据传输时触发
- `event: ping` 虽然被 SSE 解析器识别为心跳事件，但 Claude Code 的 HTTP 客户端**不会**因此重置其 body-data 超时计时器
- 改为 `data: \n\n` 后，HTTP 层有数据流动 + SSE 解析器看到 `data:` 行，双重保证客户端超时被重置

## v0.2.33

### 修复
- 修复 cogitation 阶段 ECONNRESET 问题：provider http.Client 超时从 60s 扩展到 300s，`callCtx` 同步扩展，防止长对话（300KB+ body）+ 多轮 cogitation（12-22s）导致请求超时
- 修复 handlePassthroughRawStream ctx/callCtx 传参 bug：`p.CallStream(ctx, ...)` 改为 `p.CallStream(callCtx, ...)`，与 handlePassthroughStream 保持一致
- handlePassthroughRawStream 添加 SSE 心跳（1s），上游 cogitation 期间不再静默，客户端不会超时断开
- 所有流式模式心跳间隔从 2s 缩短到 1s，降低客户端在长时间上游等待期间的超时概率
- provider http.Client IdleConnTimeout 90s→300s，匹配更长连接生命周期

### 根因
- Claude Code cogitation（模型思考阶段）期间上游发送零 SSE 数据，12-22s 静默
- 配合 Claude Code 313KB 大请求体 + 多次 cogitation，总耗时可能超过 60s
- provider http.Client 60s 超时触发 → 取消请求上下文 → lineReader 返回 context canceled → 错误事件 → ECONNRESET

## v0.2.32

### 修复
- handlePassthroughNonStreamAsSSE 心跳 goroutine 与响应写入的并发 race：close(done) 从 defer 改为 p.Call() 返回后立即执行，防止 ping 事件与 JSON 响应数据交错
- handlePassthroughNonStreamAsSSE 响应格式错误：Content-Type 为 text/event-stream 但直接写原始 JSON（带换行），现包装为 SSE 格式（event: message + compact JSON + event: done），确保客户端正确解析流式响应
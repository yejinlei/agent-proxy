## v0.2.39

### 修复
- **Thinking block delta 字段名错误（`handlePassthroughNonStreamAsSSE`）**：v0.2.38 修复了 delta 类型字符串（`thinking_delta`），但 delta 内字段名仍使用 `text` 而非 Anthropic 协议要求的 `thinking`。Anthropic Messages 协议中 thinking 块的 delta 格式为 `{"type":"thinking_delta","thinking":"..."}`，text 块的 delta 格式为 `{"type":"text_delta","text":"..."}`。现按 block 类型分别设置正确的字段名。
- **非 text/thinking 块静默发送空 delta 事件**：`content` 数组中的 `tool_use`、`image`、`tool_result` 等非文本块此前会发送 `{"type":"text_delta","text":""}`（空 delta），现直接跳过不发送。

### 根因
- v0.2.38 thinking 修复只覆盖了 delta 类型字符串，遗漏了 Anthropic 协议中不同 block 类型的 delta 字段名不同（thinking → `thinking`，text → `text`）

---

## v0.2.38

### 修复
- **`--stream-mode auto` 模式下 Claude Code 仍收到 raw JSON 而非 SSE（根因修复）**：auto 模式下的 `stream=false` 分支（`else` 路径，line 439-441）此前直接调用 `handlePassthroughNonStream`（raw JSON，无 SSE 包装、无心跳），与 v0.2.35 中只修复了显式 `non-stream` 模式的逻辑一致。Claude Code 请求（不带 `stream` 字段）在 auto 模式下被错误路由 → SSE 解析器收到纯 JSON → 超时 → ECONNRESET。现改为 `handlePassthroughNonStreamAsSSE`（非流式+SSE 包装），与 `--stream-mode non-stream` 行为一致。
- **Thinking block SSE 事件格式错误（`handlePassthroughNonStreamAsSSE`）**：上游 Anthropic Messages 响应的 `content` 数组可能含 `type:"thinking"` 的块（cogitation 内容），SSE 包装此前将其当作 text block 处理，导致三个问题：(1) `content_block_start` 含多余的 `text:""` 字段；(2) Delta 类型写死 `text_delta` 而非 `thinking_delta`；(3) 从未读取 `blockMap["thinking"]` 中的思考内容。现按 block 类型分别处理，thinking block 输出正确的 `thinking_delta` 类型并提取实际思考文本。

### 根因
- auto 模式的 else 分支：v0.2.35 修复时只覆盖了 `--stream-mode non-stream` 显式模式，遗漏了 auto 模式下 `stream=false` 的 else 路径
- Thinking block：v0.2.32 首次实现 SSE 包装时仅按 text block 逻辑写死，未考虑 Anthropic Messages 协议中 thinking block 的独立格式

---

## v0.2.37

### 修复
- **`--stream-mode non-stream` SSE 包装格式检测（`handlePassthroughNonStreamAsSSE`）**：响应格式检测覆盖 4 种协议——`content`(Anthropic Messages) → `choices`(OpenAI ChatCompletion) → `candidates`(Gemini) → `output`(OpenAI Responses) → 裸 JSON 降级，各格式输出正确的 SSE 结构（RFC 8387 unnamed event），确保 `--stream-mode non-stream` 模式下所有协议的客户端 SSE 解析器正确解析响应。
- **Alias 点/横杠归一化（`resolveAlias()`）**：Claude Code 发送模型名含点号（`claude-haiku-4.5`），alias 文件用横杠（`claude-haiku-4-5`），查找失败时 fallback 点→横杠重试。
- **`handlePassthroughStreamWithBody` 三合一修复**：添加 `\n\n` SSE 事件边界、alias 流内回显、token usage 提取，与 `handlePassthroughRawStream` 行为对齐。
- **`writeSSE` 错误处理**：任何 `w.Write` 错误均返回 false，不再仅吞没连接重置错误。
- **`handlePassthroughNonStreamAsSSE` 超时防护**：`p.Call(ctx)` 改为 `p.Call(callCtx)`（`context.WithTimeout`），防止上游卡死时连接永久挂起。
- **`buildVerboseCtx` 补全 ingress 信息**：从 context 复制 `ingressProtocol`/`providerType`，修复 raw passthrough 路径 `-v` 日志跳过问题。
- **`quickReplaceModelInBody` 改用 JSON 解析**：从字符串替换改为 `json.Unmarshal`/`json.Marshal`，避免边界匹配拼接错误。

### 根因
- SSE 包装格式检测：v0.2.32 首次实现时仅针对 Anthropic Messages，后续协议支持未同步更新包装逻辑
- Alias 点/横杠：Claude Code dot 风格（`claude-haiku-4.5`）vs 用户 kebab-case（`claude-haiku-4-5`），无归一化

---

## v0.2.36

### 修复
- **非流模式 SSE 包装格式修正（`handlePassthroughNonStreamAsSSE`）**：响应头从非标准 `event: message\ndata: {JSON}\n\nevent: done\ndata: {}\n\n` 改为标准 unnamed event 格式 `data: {JSON}\n\n`，与 Anthropic 实际 SSE 协议一致。Anthropic SSE 使用 unnamed event（仅 `data:` 行 + `\n\n` 分隔），不依赖 `event:` 字段区分事件类型，而是通过 JSON 内 `type` 字段区分。
- **移除上游请求的空 `X-Real-IP` 头（`OpenAIClient.CallStream`）**：`headers.Set("X-Real-IP", "")` 发送空值 header，语义不干净且上游会用实际客户端 IP 覆盖，功能无害但清理后更规范。

### 说明
- 本版本修复源于 BUG 审查参考意见（8 项建议），一对一核对后采纳 2 项：
  - ✅ `X-Real-IP` 空值头 → 移除
  - ✅ 非流模式 SSE 非标准 `event:` 标记 → 改为 unnamed event 格式
  - ❌ 其余 6 项经核对与分析前提不符，详见审查记录

## v0.2.35

### 修复
- **`--stream-mode non-stream` 对 Claude Code（Anthropic Messages）无效的根本原因修复**：`quickDetectStream()` 在请求体中找不到 `stream` 字段时返回 `false`，Claude Code 的 Anthropic Messages 请求体不包含该字段，导致 `if stream` 守卫拦截了所有 `--stream-mode` 分支。Claude Code 请求被路由到 `handlePassthroughNonStream`（raw JSON，无 SSE 包装、无心跳），客户端的 SSE 解析器收到纯 JSON 响应后等待 SSE 事件永不送达 → 客户端超时 → ECONNRESET。
- 修复策略：当显式设置 `--stream-mode`（非 auto）时，**不再经过 `stream` 字段判断**，直接按模式路由。Claude Code 请求现在正确到达 `handlePassthroughNonStreamAsSSE`（SSE 包装 + 500ms 心跳）。
- 新增 `quickInjectStreamFlag()`：`--stream-mode stream` 模式下，无 `stream` 字段的请求自动注入 `"stream": true`，确保上游也按流式处理。
- 新增 `handlePassthroughStreamWithBody()`：与 `handlePassthroughStream` 行为一致，但 body 在调用方已读取，避免重复读 `r.Body`。

### 根因分析
```
--stream-mode non-stream
  ↓
handleRequest()
  stream = quickDetectStream(body) → false  ← Claude Code 不带 stream 字段
  if stream {                        ← 进不来！
    switch q.streamMode {
    case "non-stream": ...          ← 死代码，Claude Code 永远到不了
    }
  } else {
    handlePassthroughNonStream(...)  ← Claude Code 实际走这里
  }
```

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
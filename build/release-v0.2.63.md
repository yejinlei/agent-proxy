## v0.2.63

### 修复

**1. Claude Code SSE 错误事件格式修复**
- `sendSSEErrorBody` 和 `sendSSEErrorFromUpstream` 在发送 `event: error` 前补发 `message_start` 事件
- Claude Code 解析器要求 SSE 流以 `message_start` 开头，否则报 "API returned an empty or malformed response (HTTP 200)"
- `message_stop` 事件添加 `event: message_stop` 前缀（Anthropic 标准 SSE 事件格式）
- 修复上游返回错误（如 HTTP 400）时 Claude Code 无法解析 SSE 错误事件的问题

**2. Codex Responses 翻译器补发 response.completed（v0.2.62 修复重新编译）**
- Responses 翻译器在 events channel 关闭或 ctx 取消时，补发 `response.completed` 事件再发 `[DONE]`
- 修复上游 OpenAI SSE 以 `[DONE]` 结束但无 FinishReason 时 Codex 报 "stream disconnected before completion: stream closed before response.completed"
- v0.2.62 的 GitHub release 二进制可能未包含此修复，v0.2.63 重新编译确保包含

**3. 并发写防护（v0.2.62 修复重新编译）**
- `MutexSSEWriter` 防止心跳 goroutine 与 TranslateStream 回调并发写 `http.ResponseWriter`
- `handleStreamRequest`（quick.go + gateway.go）使用 `MutexSSEWriter` 保护 SSE 写入

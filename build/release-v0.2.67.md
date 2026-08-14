## v0.2.67

### 修复

**1. SSE 错误事件 message_start 的 id/model 为空导致 Claude Code 报 "empty or malformed response"**
- `handlePassthroughStreamWithBody` 的 `_type: error` 处理：`message_start` 事件的 `id` 和 `model` 从空字符串改为正确值
- `sendSSEErrorBody` 和 `sendSSEErrorFromUpstream`：`message_start` 事件的 `id` 从空字符串改为 `msg_<timestamp>`
- gateway.go 同步修复 2 处错误路径的 `message_start` 事件

**2. handlePassthroughNonStreamAsSSE 缺少 fixNullUsageInResponse**
- 透传非流式→SSE 包装路径未调用 `fixNullUsageInResponse`，导致 usage 为 null 时 Claude Code 报 `K.usage.input_tokens undefined`

**3. Codex 流式错误路径缺少 response.created（v0.2.66 已修复，本次确认）**
- `handleStreamRequest` OpenAI 兼容路径：收到上游 `_type: error` 时，先发送 `start` 事件
- Responses 翻译器：`start` 事件始终发送 `response.created`（即使 ID 为空）
- gateway.go 同步修复

### 测试
- 全部单元测试通过（server + protocol，共 23s）

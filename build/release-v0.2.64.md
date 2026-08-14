## v0.2.64

### 修复

**1. Codex 流式响应缺少 response.created 事件**
- OpenAI 兼容路径（CC 上游）在首个有效 delta 前补发 `InternalStreamEvent{Type:"start"}`
- Responses 入站翻译器据此生成 `response.created` 事件，完成完整事件序列
- 修复 Codex 报 "stream disconnected before completion: stream closed before response.completed"

**2. Claude Code 模型验证失败（usage 为 null）**
- Anthropic `TranslateResponse` 在上游 usage 为 nil 时，提供默认空对象 `{InputTokens:0, OutputTokens:0}`
- 修复 `/model` 命令验证模型时报 `undefined is not an object (evaluating 'K.usage.input_tokens')`

**3. 透传流式错误事件格式修复**
- `handlePassthroughStream` 错误处理路径补发 `message_start` 前缀
- `message_stop` 添加 `event:` 前缀（Anthropic 标准 SSE 事件格式）
- 修复上游流式错误时 Claude Code 报 "empty or malformed response"

### 测试
- 全部单元测试通过（server + protocol）
- 心跳测试通过（`TestHeartbeatDuringLongUpstreamCall`）

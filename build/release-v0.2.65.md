## v0.2.65

### 修复

**1. gateway.go 同步修复（quick.go ↔ gateway.go 双模式同步）**
- `handlePassthroughNonStream` 添加 `fixNullUsageInResponse`：透传路径修复 usage 为 null 的问题
- `handleStreamRequest` 错误路径添加 `message_start` + `event: message_stop`
- `handleStreamRequest` OpenAI 兼容路径添加 `ccStartSent` + `start` 事件
- `handlePassthroughStream` 错误路径添加 `message_start` + `event: message_stop`

**2. Claude Code /model 验证失败（usage null → undefined）**
- 透传非流式路径 `handlePassthroughNonStream` 新增 `fixNullUsageInResponse` 函数
- 当上游响应中 `"usage": null` 时，自动补为 `{"input_tokens":0,"output_tokens":0}`
- 修复 Claude Code 报 `undefined is not an object (evaluating 'K.usage.input_tokens')`

**3. Codex 流式响应缺少 response.created 事件**
- OpenAI 兼容路径（quick.go + gateway.go）在首个有效 delta 前补发 `InternalStreamEvent{Type:"start"}`
- Responses 入站翻译器据此生成 `response.created` 事件
- 修复 Codex 报 "stream disconnected before completion: stream closed before response.completed"

**4. SSE 错误事件格式统一**
- 所有 SSE 错误路径（透传/翻译、quick.go/gateway.go）统一添加 `message_start` 前缀
- `message_stop` 统一添加 `event: message_stop` 前缀（Anthropic 标准 SSE 事件格式）

### 测试
- 全部单元测试通过（server + protocol，共 23s）
- 心跳测试通过

## v0.2.66

### 修复

**1. Codex 流式错误路径缺少 response.created（根因修复）**
- `handleStreamRequest` OpenAI 兼容路径：收到上游 `_type: error` 时，如果 `ccStartSent` 为 false，先发送 `start` 事件
- Responses 翻译器收到 `start` 事件后生成 `response.created`，确保 Codex 收到完整事件序列
- 修复 Codex 报 "stream disconnected before completion: stream closed before response.completed"

**2. Responses 翻译器 start 事件 ID 为空时不发送 response.created**
- 之前：`start` 事件只有 `event.Data.ID != ""` 时才发送 `response.created`
- 现在：`start` 事件始终发送 `response.created`（即使 ID 为空）
- 错误路径中的 `start` 事件没有 ID，导致 `response.created` 不发送

**3. gateway.go 同步修复（双模式同步）**
- `handleStreamRequest` 添加 `_type: error` 处理（之前完全缺失）
- 错误路径添加 `ccStartSent` + `start` 事件

### 测试
- 全部单元测试通过（server + protocol）

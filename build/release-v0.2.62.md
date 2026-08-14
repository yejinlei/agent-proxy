## v0.2.62

### 修复

**1. Claude Code 心跳格式修复**
- 心跳从 `data: {"type":"ping"}` 改为 `event: ping` + `data: {"type":"ping"}`
- 添加 `event: ping` 前缀，Claude Code SSE 解析器能正确识别为内容活动事件，重置 HTTP 超时
- 修复长上游响应（>2min）时 Claude Code 报 "API returned an empty or malformed response (HTTP 200)"

**2. Codex Responses 翻译器补发 response.completed**
- Responses 翻译器在 events channel 关闭或 ctx 取消时，补发 `response.completed` 事件再发 `[DONE]`
- 修复上游 OpenAI SSE 以 `[DONE]` 结束但无 FinishReason 时 Codex 报 "stream disconnected before completion: stream closed before response.completed"
- 与其他 3 个翻译器（Anthropic/Gemini/ChatCompletion）的 channel 关闭兜底逻辑对齐

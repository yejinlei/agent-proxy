## v0.2.102

### 修复

**Responses → OpenAI CC 翻译时 `developer` role 导致上游 400**

**根因：** Codex（Responses 协议）入站请求包含 `role:"developer"` 消息。Responses API 原生支持 developer 角色，但标准 OpenAI CC 端点（`/v1/chat/completions`）大多数上游不支持（Sensenova 文档无 developer 提及），原样透传导致 `400 Invalid request format`。

数据流：
```
Codex Responses input[role:"developer"]
  → responses/translator.go:151  msg.Role = "developer"   （原样透传，正确）
  → gateway.go buildCCRequest    Role = string(msg.Role)  （原样输出，错误）
  → Sensenova /v1/chat/completions 收到 role:"developer" → 400
```

**修复：** 在 `internal/server/gateway.go` 的 `buildCCRequest` 中加入 `mapRoleToCC()` 角色映射函数，将 `developer` 映射为 `system`（语义最接近——都是给模型的指令）。其他 role（user/assistant/tool）原样输出。`gateway.go` 和 `quick.go` 共享 `buildCCRequest`（同属 `package server`），一处修改两模式同时生效。

**路由策略确认：**
- 入站协议 ∈ 上游 capabilities → **透传**（原样转发，仅替换 model 名）
- 入站协议 ∉ 上游 capabilities → **翻译**，出站协议优先级：**openai（最大公约数）> gemini**
- CC 是所有上游的通用协议，developer→system 语义损失极小

**未改动项（已确认）：**
- `responses/translator.go:811` 不动——Responses 协议本身支持 developer，Codex 原生格式不需映射
- `selectProtocol` 不变——透传/翻译决策逻辑已验证正确

**测试：** 新增 `TestBuildCCRequest_DevRoleMapToSystem` 验证 developer→system。修复已存在的 `e2e_test.go` 调用签名不匹配。`go test ./internal/server` 全通过。
# v0.2.105

## 修复

### 1. Sensenova `top_p=0` / `max_tokens=0` → HTTP 400

**问题：** 当 Sensenova 作为上游时，如果请求体包含 `top_p:0` 或 `max_tokens:0`，Sensenova 返回 HTTP 400：

```
{"error":{"message":"field TopP invalid, should be in (0, 1]"}}
{"error":{"message":"field MaxTokens invalid, should be in [1, 65536]"}}
```

**根因：** Sensenova 对 `top_p` 和 `max_tokens` 有严格的范围校验，但 agent-proxy 在所有路径中原样透传，没有做兼容处理。

**修复（三处）：**

| 路径 | 文件 | 修复 |
|------|------|------|
| CC 上游请求构造 | `internal/server/gateway.go` `buildCCRequest` | `top_p≤0` 时不发送，`max_tokens≤0` 时不发送 |
| Responses 上游请求构造 | `internal/protocol/responses/translator.go` `TranslateToProvider` | `top_p≤0` 时不发送 |
| CC 透传路径 | `internal/server/quick.go` `stripSensenovaIncompatible` | 递归删除 `top_p≤0` 和 `max_tokens≤0` |

**验证：** 通过 curl 对 Sensenova 真实端点做端到端验证，三条路径全部 HTTP 200 ✅

### 2. Sensenova 工具调用

**问题：** Sensenova 的 CC 端点支持 `tools` 和 `tool_choice`，但 agent-proxy 的 `buildCCRequest` 没有传递 `ToolsChoice` 字段。

**状态：** 已通过 curl 确认 Sensenova 完整支持工具调用，工具定义传递无问题。
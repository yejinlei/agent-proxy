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
- 路径 1（CC 透传）: `POST /v1/chat/completions` + `top_p:0` → HTTP 200
- 路径 2（Responses→CC 翻译）: `POST /v1/responses` + `top_p:0` → HTTP 200
- 路径 3（Codex 全量请求）: `POST /v1/responses` + `top_p:0` + `max_output_tokens:0` + `tools` + `instructions` → HTTP 200

**生产验证（2026-08-23 23:16）：** 代理已部署 v0.2.105-test，三条路径全部通过 ✅

**⚠️ 生产部署关键教训：旧进程残留导致修复无效**

部署 v0.2.105 后，代理仍报 `field TopP invalid`，即使源码修复已合入。根因：

```
PID 1376896 — /tmp/agent-proxy (启动 21:59，旧版 v0.2.105-test，未包含 filter)
PID 1234827 — /tmp/agent-proxy (启动 10:48，旧版)
```

`pkill -f agent-proxy` 在 Windows 环境（Git Bash）下**未生效**（Windows taskkill 路径问题），新二进制虽重建成功但进程未重启。curl 请求仍命中旧进程。

**修复方法：** 用 `kill -9 <PID>` 逐个清理所有残留进程后重启。

**部署检查清单（新增）：**
1. `ps aux | grep agent-proxy | grep -v grep` — 确认旧进程已清理
2. `stat -c "%y" /tmp/agent-proxy` — 确认二进制是最新版
3. `curl /health` — 确认代理已重启
4. 验证 curl 日志中 `[strip]` 或 `[provider]` 出现，证明请求经过 filter

### 2. Sensenova 工具调用

**问题：** Sensenova 的 CC 端点支持 `tools` 和 `tool_choice`，但 agent-proxy 的 `buildCCRequest` 没有传递 `ToolsChoice` 字段。

**状态：** 已通过 curl 确认 Sensenova 完整支持工具调用，工具定义传递无问题。
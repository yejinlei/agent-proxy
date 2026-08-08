## v0.2.14 — Anthropic 协议认证修复

### What changed

- **fix**: `AnthropicClient.DefaultHeaders` 同时发送 `Authorization: Bearer <key>` 和 `x-api-key: <key>` 两个认证头。之前只发送 `x-api-key`，导致兼容网关（如 `token.sensenova.cn`）返回 401，因为该类网关只识别 `Authorization` 头
- **feat**: 新增 `ProxyRecord.ModelsMap()` 辅助方法，解析 `models_map_json` 字段
- **refactor**: `NewQuickGateway` 签名增加 `modelsMap` 参数，为后续多上游支持预留
- **refactor**: `selectProtocol` 签名增加 `model` 参数，路由逻辑保持基于 capabilities 的协议匹配（与模型无关）

### Root cause

Anthropic 原生 API 使用 `x-api-key` 头认证，但 OpenAI 兼容网关（如商汤 Sensenova）使用 `Authorization: Bearer <key>`。当客户端通过 Anthropic 协议接入、上游为兼容网关时，仅发送 `x-api-key` 会被网关拒绝。

修复：同时发送两个头。原生 Anthropic API 和兼容网关都能正确处理，无兼容性风险。

### 兼容性

- 无 API 变更
- 仅影响 `AnthropicClient.DefaultHeaders`
- 对原生 Anthropic API 和兼容网关均兼容

### Assets

| Platform | Binary | SHA256 |
|----------|--------|--------|
| Linux amd64 | agent-proxy-linux-amd64 | bb16c1046fc034830754db1dba5fea3da8e988dda9f14ebf9c949992f0fd2f7b |
| Linux arm64 | agent-proxy-linux-arm64 | 71a1864354078be4f6bde5f53b815b374773c71e7498f6af10192da3a51eb887 |
| Windows amd64 | agent-proxy-windows-amd64.exe | 6cef34755998b4550701c2d34db2e407eb08e6df0fab587951f69adba16364be |
| Windows arm64 | agent-proxy-windows-arm64.exe | e93d24fd67c97291b605440ea500fc6e91b9da8b49e03976fa4843e9814ce1f3 |
| macOS amd64 | agent-proxy-darwin-amd64 | 1884a2bc8124462502cec4fe1d537e0e52f59708008179b0300181acaed4d912 |
| macOS arm64 | agent-proxy-darwin-arm64 | 67eacc66f3e9e04c4647cb22925c472cd414141117d10641d6d945d4e012b227 |
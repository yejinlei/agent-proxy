# v0.2.8 — 协议修复

## Bug 修复

- **Anthropic Messages content: null** — TranslateToProvider 不再强制 max_tokens >= 1024。商汤 Anthropic 兼容端点不强制此最小值，传入被强制的 max_tokens 导致上游返回空内容。直接透传 req.MaxTokens。
- **Gemini parts: [{}]** — 同上根因。providerType="anthropic" 时所有入口协议（含 Gemini）均经 AnthropicTranslator.TranslateToProvider，被强制的 max_tokens 导致返回空 parts。随 Anthropic 修复一并解决。
- **Responses HTTP 500** — inputToMessages 在 type 字段为空时默认 "message"。部分客户端省略 type 导致全部消息被跳过，触发 500。

## 变更

- internal/protocol/anthropic/translator.go — TranslateToProvider 移除 max_tokens 强制逻辑
- internal/protocol/responses/translator.go — inputToMessages 补全 type 默认值

## 二进制

| 平台 | 文件名 | SHA256 |
|------|--------|--------|
| Linux x86_64 | agent-proxy-linux-amd64 | fde71a6e... |
| Linux ARM64 | agent-proxy-linux-arm64 | 1d54ffb1... |
| Windows x86_64 | agent-proxy-windows-amd64.exe | e94bcae0... |
| Windows ARM64 | agent-proxy-windows-arm64.exe | 6379c748... |
| macOS x86_64 | agent-proxy-darwin-amd64 | d1766191... |
| macOS ARM64 | agent-proxy-darwin-arm64 | 4e208718... |

完整 SHA256 见 SHA256SUMS.txt。

## 提交

33f035e — fix: Anthropic max_tokens forcing and Responses missing type field

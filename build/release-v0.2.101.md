## v0.2.101

### 修复

**上游非确定性错误自动重试（400/408/429/5xx）**

**根因：** Sensenova 等 OpenAI 兼容上游对相同请求体存在非确定性行为——同一请求体在毫秒级间隔内有时返回 200 有时返回 400 `Invalid request format`。这是上游服务端负载均衡/限流波动，非 agent-proxy 翻译逻辑问题。

**修复：** 在 `internal/provider/openai.go` 的 `OpenAIClient.Call` 和 `OpenAIClient.CallStream` 中加入最多 2 次指数退避重试（100ms → 400ms），覆盖 400/408/429/5xx 状态码。重试逻辑位于 provider 层，对上层调用方完全透明，两条路径（透传 Passthrough + 翻译 Translation）自动受益。

**参考设计：** Anthropic Translator 的 `TranslateStream` 错误处理路径直接透传错误不做状态机重试，同理将重试逻辑放在 provider 层而非 translator 层，保持分层清晰。

**重试日志：** 每次重试输出 `[retry] attempt N/M, waiting 100ms (model=...)` + `[retry] HTTP 400 on attempt N/M, body=...`，便于诊断。

**覆盖范围：** 仅 `OpenAIClient`（OpenAI Chat Completions + Responses 端点）。`AnthropicClient` 和 `GeminiClient` 未修改。
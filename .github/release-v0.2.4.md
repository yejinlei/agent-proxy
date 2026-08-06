# v0.2.4 — Gemini 模型路径透传修复

**真正修复了 Gemini 协议：`/v1/models/{model}:generateContent` 路径中的模型名现在能正确透传到上游。**

## 修复

- **Gemini 模型名从路径提取** — 当请求 body 不含 `model` 字段时（标准 Gemini 客户端通过路径 `/v1/models/{model}:generateContent` 传递模型），从路由上下文读取模型名并注入，不再硬编码 `gemini-1.5-flash`
- **TranslateRequest 接口升级** — 所有协议翻译器（Anthropic、ChatCompletion、Responses、Gemini）的 `TranslateRequest` 新增 `ctx context.Context` 参数，支持请求路径信息在翻译链路中透传

## 用法

```powershell
.\agent-proxy.exe run --db 1
.\agent-proxy.exe run --db 1 --host 0.0.0.0
```

## 二进制

- `agent-proxy-darwin-amd64`
- `agent-proxy-darwin-arm64`
- `agent-proxy-linux-amd64`
- `agent-proxy-linux-arm64`
- `agent-proxy-windows-amd64.exe`
- `agent-proxy-windows-arm64.exe`

# v0.2.2 — Responses 协议支持 + Gemini 路由修复

**新增 OpenAI Responses API 端点支持，修复 Gemini `:generateContent` 冒号路径 404。**

## 新特性

- **OpenAI Responses 协议 (`/v1/responses`)** — 新增 `NewResponsesClient` provider 客户端，`provider_type: "responses"` 可直接使用，入站/出站翻译链路已完整
- **GET `/v1/models`** — 透传上游实时模型列表（不再用硬编码），支持 agent-nexus 自动发现

## 修复

- **Gemini `:generateContent` 路径 404** — chi 路由器不识别冒号作为路径分隔符；用 `/v1/models/*` 通配 + 尾缀判断 `:generateContent` 的 `handleModelsCatchAll` 替代失效的正则路由，标准 Gemini `POST /v1/models/{model}:generateContent` 现在正常路由
- **Chat Completions 消息内容丢失** — `Content` 字段在 `messageToInternal` 被丢弃，导致上游收到空消息
- **响应 content 为空** — `chatCompletionToInternal` 用裸字节存储 content，出站翻译无法反序列化

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

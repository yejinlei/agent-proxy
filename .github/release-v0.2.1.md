# v0.2.1 — 模型列表 + 内容修复

**快速模式新增 `GET /v1/models` 透传上游模型列表，修复 Chat Completions 消息内容丢失 bug。**

## 新特性

- `GET /v1/models` — 透传上游实时模型列表（不再用硬编码）
- 支持 agent-nexus 自动发现模型

## 修复

- **Chat Completions 消息内容丢失** — `Content` 字段在 `messageToInternal` 被丢弃，导致上游收到空消息
- **响应 content 为空** — `chatCompletionToInternal` 用裸字节存储 content，出站翻译无法反序列化

## 用法

```powershell
.\agent-proxy.exe run --db 1
.\agent-proxy.exe run --db 1 --host 0.0.0.0
```

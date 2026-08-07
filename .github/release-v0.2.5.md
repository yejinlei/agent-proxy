# v0.2.5 — 透传优化 + Gemini/Responses 修复 + 测试套件

**同协议入站 → 同类型 Provider 出站时，网关现在跳过翻译链路直接透传原始请求体，零损耗、零开销。**

## 优化

- **透传路径（Passthrough）** — 当入站协议 == 下游 Provider 类型时，请求体直接通过 `ProviderClient.Call` / `CallStream` 转发，不经翻译器。模型名通过防御拷贝的 `ProviderInfo.Name` 注入下游 URL
- **防御拷贝** — 路由返回的共享 `ProviderInfo` 在透传和翻译两条路径上各自拷贝，避免被下游误改

## 修复

- **Gemini 透传 URL 模型名** — `GeminiClient.Call` / `CallStream` 现在从 `info.Name` 读取模型名构造 `/v1/models/{model}:generateContent`，修复透传模式下模型名缺失的 bug
- **导出 `GeminiModelFromContext`** — 透传路径可从路由上下文读取路径中的模型名（`/v1/models/{model}:generateContent`）
- **Responses API input 字段** — 支持 `input` 为纯字符串（单消息）或 `[]InputItem` 数组两种形式
- **ChatCompletion 流式 Reasoning** — `StreamDelta` 新增 `reasoning` 字段

## 新增

- **全协议正确性测试套件** — 覆盖 Provider 客户端（Gemini URL 构造）、Gemini 翻译器（context 模型、body→schema→provider→body 往返）、Server 辅助函数（防御拷贝、模型提取、stream 检测、错误格式）

## 用法

```powershell
.\agent-proxy.exe run --db 1
.\agent-proxy.exe run --db 1 --host 0.0.0.0 --port 8080
```

## 二进制

- `agent-proxy-darwin-amd64`
- `agent-proxy-darwin-arm64`
- `agent-proxy-linux-amd64`
- `agent-proxy-linux-arm64`
- `agent-proxy-windows-amd64.exe`
- `agent-proxy-windows-arm64.exe`

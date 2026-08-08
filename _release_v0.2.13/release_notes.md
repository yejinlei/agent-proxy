## v0.2.13 — 流式 SSE 事件分隔符修复

### What changed

- **fix**: `handlePassthroughStream` 写回 SSE 行时缺少事件分隔符空行（`\n\n`），之前只写了一个 `\n`，导致 TraeCode 及其他 SSE 客户端的 `onmessage` 事件永远不触发，流式回复显示为空白

### Root cause

SSE（Server-Sent Events）协议要求每个事件以**空行**结束：

```
data: { "content": "Hello" }

<空行>
```

之前的实现写回时是 `w.Write([]byte("\n"))`，只写了一个换行符，没有空行。SSE 客户端不会解析不完整的事件，因此 TraeCode 一直等不到事件完成信号。

修复：改为 `w.Write([]byte("\n\n"))`。

### 兼容性

- 无 API 变更，纯 bug 修复
- 仅影响 `handlePassthroughStream`（快速模式透传流式路径）

### Assets

| Platform | Binary | SHA256 |
|----------|--------|--------|
| Linux amd64 | agent-proxy-linux-amd64 | 519ba7674ef41466e55fae3024e700c87f78f667f27d0c2d6dae562c47f54151 |
| Linux arm64 | agent-proxy-linux-arm64 | 54e4be6505749444365bfd204ca40e78b9da6040cdb2fe49816e909cab659ab3 |
| Windows amd64 | agent-proxy-windows-amd64.exe | a5955017e96c1de34582be1540ab89ac4c0dbf17aa374f3f454c5b25159c32fd |
| Windows arm64 | agent-proxy-windows-arm64.exe | 7bf8d6c9ce1420d79465ee75e9551b2d55f33e492d4c5520e0e1eb35a33f0bd7 |
| macOS amd64 | agent-proxy-darwin-amd64 | b8fa7492d34c8c34fdb2a701c1facafd49c9afd2d87311cbee0962855e014d1d |
| macOS arm64 | agent-proxy-darwin-arm64 | eb0ca6dd37a86d5a20ab18227c160b5b74bc99b2b7fe3150fac0fb7fb2a59ed8 |
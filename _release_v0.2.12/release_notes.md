## v0.2.12 — 流式 SSE 解析修复 + 全协议 E2E 测试

### What changed

- **fix**: `lineReader()` 在 `internal/provider/openai.go` 中移除了 `needFlush` 标志——该标志在首次找到换行符后，导致后续所有 SSE 行被拼接成一条，使 Anthropic / Gemini / Responses 的流式翻译全部丢失数据块
- **fix**: `handleStreamRequest` 在传入翻译器前剥离 `data: ` 前缀（与 OpenAI 透传路径一致），避免翻译器因前缀无法解析 JSON
- **test**: 新增 8 个 E2E 子测试（`internal/server/e2e_test.go`），覆盖 4 种协议 × 流式/非流式，通过 mock `httptest.Server` 验证 QuickGateway 全链路

### 8 个 E2E 测试

| 协议 | 模式 | 状态 |
|------|------|------|
| ChatCompletion | non-stream / stream | PASS |
| Anthropic | non-stream / stream | PASS |
| Gemini | non-stream / stream | PASS |
| Responses | non-stream / stream | PASS |

### Root cause

```go
// BUG: needFlush 在首次 \n 后设为 true，下次调用直接返回整个缓冲区剩余内容，
// 不再扫描 \n，导致多个 SSE 行拼接成一条
if needFlush && pos > 0 {
    data := make([]byte, pos)
    copy(data, buf[:pos])
    pos = 0
    needFlush = false
    return data, nil  // ← 返回所有剩余数据，跳过换行符扫描
}
```

修复：移除 `needFlush`，每次循环先扫描换行符再读取数据，`continue` 回到扫描阶段。

### 兼容性

- 无 API 变更，纯 bug 修复
- `lineReader` 行为修正后与标准 SSE 解析一致
- `data: ` 前缀剥离不影响无前缀的输入（`strings.TrimPrefix` 安全）

### Assets

| Platform | Binary | SHA256 |
|----------|--------|--------|
| Linux amd64 | agent-proxy-linux-amd64 | 4f8d205b42e4cbc8f456be72ac8491f7186f7979d78983aa4ed9af4689115800 |
| Linux arm64 | agent-proxy-linux-arm64 | b53b266cc5a3f99965145adffe3168bcfc3dcb4131c259ae8391658acd9bfbb4 |
| Windows amd64 | agent-proxy-windows-amd64.exe | 34da1a42499a8cb172c9a28e32c8d79e792f4bd0530c21fd69ef086e0366f295 |
| Windows arm64 | agent-proxy-windows-arm64.exe | 9687de572ee33054ab3430b29ec5caeeb93fb4c9c5e6b34b44f63a0b152a8133 |
| macOS amd64 | agent-proxy-darwin-amd64 | 80f572748d24709ce4db3b67565438f33433e5a4c486aab49b8992bf323842ae |
| macOS arm64 | agent-proxy-darwin-arm64 | a945d099c0c6155cbd17e62e4eac850dd6fd8afb896b3980c1e0809d0dbdf185 |
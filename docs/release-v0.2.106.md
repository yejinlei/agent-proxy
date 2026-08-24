# v0.2.106

## 修复

### 1. Codex（Responses→CC 翻译路径）→ Sensenova HTTP 400 "Invalid request format"

**问题：** 使用 Codex（OpenAI Responses 协议）经代理调用 sensenova 上游时，Sensenova 立即返回 HTTP 400：

```
{"error":{"message":"Invalid request format","type":"invalid_request_error","code":"BadRequest"}}
```

该 400 为即时返回（约 5µs），并非超时。两条不同的请求（input_items=0 / 28057B、input_items=3 / 60990B）均稳定复现，错误完全一致。

**根因：** 翻译路径（Responses→Central Schema→CC）由 `buildCCRequest` 构造上游请求体，该函数存在两个 sensenova 不兼容的问题，且**透传路径的 `stripSensenovaRequestFields` 过滤对翻译路径完全不起作用**（DB 中该 provider 的 `upstreamType` 存为 `"openai"`，非 `"sensenova"`，故 filter 不触发）：

1. **`stream_options:{include_usage:true}` 无条件注入（主因）** — `buildCCRequest` 对所有上游无条件注入该字段（Claude Code 依赖 usage 数据），但 sensenova CC 端点拒绝该未知字段 → `Invalid request format`。该字段存在于每一次翻译请求中，故两条不同请求均失败一致。
2. **空 `Tool{}` 槽（次因）** — `make([]Tool, len(req.Tools))` 预分配，当 `Function==nil` 时序列化为 `{"type":"","function":null}`，sensenova 同样可能拒绝。

**诊断难点：** 日志中 `[代理 → LLM]` 请求体被 `formatJSON()` 的 20KB 截断限制砍断，尾部（tools 数组、stream_options 所在位置）在日志中不可见，导致初次分析无法直接看到 offending field，只能从 `buildCCRequest` 源码 + 两条请求的公共字段集反推。

**修复（`buildCCRequest`，gateway.go / quick.go 同步）：**

| 修复 | 文件 | 说明 |
|------|------|------|
| 按上游自适应 `stream_options` | `internal/server/gateway.go` `buildCCRequest` | 新增 `baseURL` 参数，经 `DetectUpstreamType` 检测：sensenova 上游省略 `stream_options`；非 sensenova 上游保留（Claude Code usage 不受影响） |
| 空 Tool 槽过滤 | `internal/server/gateway.go` `buildCCRequest` | 改用 `append` 构造 tools 切片，跳过 `Function==nil` 的槽位 |
| 调用点同步 | `internal/server/quick.go` `translateToProvider` | 两处调用点传入 `q.info.BaseURL` |

**关键设计：翻译路径与透传路径的过滤职责分离。** 透传路径由 `stripSensenovaRequestFields`（基于 DB `upstreamType`）过滤；翻译路径在 `buildCCRequest` 内按 `baseURL` 域名检测，不依赖 DB 字段。两者互补，确保 sensenova 在所有路径上均兼容。

**验证：**
- `go test ./internal/server` → 全量 PASS（19.3s）
- `TestCodex_SensenovaStreamOptionsFilter`：sensenova 上游 `stream_options` 已省略 ✅；非 sensenova 上游保留 ✅
- `TestBuildCCRequest_EmptyToolSlotFiltered`：空 Tool 槽已过滤，无 `{"type":"","function":null}` ✅
- `go build ./...` → exit 0

**⚠️ 部署注意：** 代码修复必须配合进程重启才能生效。v0.2.105 曾因旧进程残留导致"看似修好实则未生效"。部署后务必确认运行的进程是新版二进制（见 v0.2.105 部署检查清单）。
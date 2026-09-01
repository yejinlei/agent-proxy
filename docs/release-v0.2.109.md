# v0.2.109

## 修复

### 1. Codex 报 `Invalid request format`（内置工具被包成空名 function）

**问题：** 使用 Codex 经 WebSocket 走 Responses 协议时，上游 Sensenova 返回：

```
HTTP 400 {"error":{"message":"Invalid request format"}}
```

**根因：** Codex 发来的 `tools` 数组末尾混入了两类**客户端执行的内置工具**：

```json
{"type":"tool_search","execution":"client",...}
{"type":"web_search","external_web_access":false}
```

这些不是自定义函数，`types.go` 的 `Tool.UnmarshalJSON` 因没有 `function` 字段走 else 分支，只设置 `Type`/`Parameters`，**`Name` 留空**。`TranslateRequest` 此前无条件把所有 tool 包成 `{"type":"function","function":{"name":"",...}}` —— 空函数名是非法定义，Sensenova 直接 400。

日志证据（`agent-proxy-9091.log`）：上游请求体里能看到对那两个内置工具生成了 `"function":{"name":""}` 的空名定义。

**修复：** `TranslateRequest` 的 tools 循环加过滤，只把 `type=="function"` 且 `name` 非空的 tool 转发上游，其余（`tool_search`/`web_search` 等客户端内置工具）丢弃。

```go
for _, tool := range req.Tools {
    if tool.Type != "function" || tool.Name == "" {
        continue
    }
    // ... 包成 InternalTool
}
```

内置工具由 Codex 客户端自己执行，本就不应转发给上游模型。

### 2. Codex 报 `No user query found in messages`（连接预热空 input）

**问题：** Codex 建立连接时发预热请求（`input:[]` / `generate:false` / `request_kind:"prewarm"`），上游 Sensenova 返回：

```
HTTP 400 {"error":{"message":"Failed to build prompt: No user query found in messages."}}
```

**根因：** 预热请求的 `input` 为空数组，`inputToMessages` 转换后只剩从 `instructions` 来的 `system` 消息，**没有 user 消息**。Sensenova 要求至少一条 user 消息才肯构建 prompt，于是 400。

日志证据（`agent-proxy-9091.log`）：`[CODEX-DEBUG] TranslateRequest: ... input_items=0 messages=0`，紧接上游 400 "No user query found in messages"。

**修复：** `TranslateRequest` 在 `inputToMessages` 之后检查 `messages` 是否为空，为空则注入一条中性 user 消息兜底（content 为空串），让上游接受请求并正常返回。

```go
if len(messages) == 0 {
    emptyContent, _ := json.Marshal("")
    messages = []schema.InternalMessage{{
        Role:    schema.RoleUser,
        Content: emptyContent,
    }}
}
```

| 文件 | 改动 |
|------|------|
| `internal/protocol/responses/translator.go` | `TranslateRequest`：tools 循环过滤内置/空名工具（Fix 1）；空 input 注入 user 消息兜底（Fix 2）；新增 `@AI_GUARD: RESPONSES_FILTER_BUILTIN_TOOLS`、`@AI_GUARD: RESPONSES_EMPTY_INPUT_USER_FALLBACK` 标记 |
| `main.go` | 版本号 `v0.2.108` → `v0.2.109` |

**双模式同步说明：** `TranslateRequest` 通过 `CombinedTranslator` 接口被调用，`quick.go`（快速模式）和 `gateway.go`（复杂模式）共享同一份实现，无需重复修复。

**验证：** `go build ./...` 通过；`go test -count=1 ./internal/protocol/responses/... ./internal/server/...` 全部通过。

# v0.2.110

## 修复

### 1. Codex 工具调用失效（pwd 等工具返回空参数）

**问题：** 使用 Codex CLI（v0.151.0）经 agent-proxy 走 Responses 协议接入 OpenAI 兼容上游（如 Sensenova）时，模型发起的工具调用参数为空 —— 例如 `pwd` 工具返回空 Path，Codex 侧表现为工具执行结果异常。聊天文本正常，仅 tool_calls 受损。

**根因（`internal/protocol/responses/translator.go` 的 `TranslateStream`，共 4 个缺陷叠加）：**

1. **幽灵函数调用条目**：CC 协议中 tool_call 的 ID/Name 只出现在第一个分片，后续分片是**空 ID** 的参数增量。旧逻辑为每个空 ID 分片合成 `synth-N` 假条目，导致同一工具调用被拆成多个 output item，出现未配对的 `output_item.added`、重复的 `fc_arguments.done`（参数为 `"{}"`）。Codex 按错误条目绑定参数 → 拿到空 `{}`。
2. **缺少 per-fc `output_item.done`**：函数调用的 `output_item.added` 发出后，收尾时没有对应的 `output_item.done`，违反「每个 added 必须配对一个 done」的 Responses SSE 生命周期约束。
3. **`done` 事件内嵌内容被丢弃**：部分上游（如 sensenova-6.8-flash-lite）把完整 tool_calls / content 打包在带 `finish_reason` 的最后一条 chunk 里。旧逻辑对 `eventType="done"` 的事件直接跳过，工具调用数据整个丢失。
4. **`output_item.added` 的 `arguments` 字段类型错误**：写成了 JSON 对象而非字符串，不符合 Responses API 规范。

**修复：**

- `getFC`：空 ID 增量合并进最近一个已注册函数调用（不再合成假条目）；晚到的 Name 合并到已知条目。新增 `@AI_GUARD: RESPONSES_FC_KEYING`。
- 新增 `closeFuncCall`：保证每个函数调用恰好一次 `response.function_call_arguments.done` + 一次 `response.output_item.done`（`argsDone`/`itemDone` 守卫），最终参数解析为对象嵌入 item。新增 `@AI_GUARD: RESPONSES_FC_TEARDOWN`。
- 新增 `finalizeItems`：统一收尾顺序 —— 先关闭 message item（`output_text.done` + `output_item.done`），再逐个关闭函数调用 item。新增 `@AI_GUARD: RESPONSES_ITEM_ORDER`。
- `case "done":` 处理内嵌的 `Message.Content` 与 `Message.ToolCalls`（与 delta 分支同路径），随后进入收尾。新增 `@AI_GUARD: RESPONSES_DONE_EMBEDDED_CONTENT`。
- `sendOutputItemAddedFunc` 的 `arguments` 改为字符串 `""`。
- 错误分支（v0.2.108 引入的完整收尾）改用 `finalizeItems("failed")`，同样覆盖函数调用收尾。

| 文件 | 改动 |
|------|------|
| `internal/protocol/responses/translator.go` | 重写 `TranslateStream` 函数调用状态机与收尾序列（上述 6 项） |
| `main.go` | 版本号 `v0.2.109` → `v0.2.110` |

**双模式同步说明：** `TranslateStream` 通过 `CombinedTranslator` 接口共享，`quick.go:2912`（快速模式）与 `gateway.go:1167`（复杂模式）调用同一实现，单点修复自动覆盖两种模式。已确认 gateway.go 无内联复制逻辑。

**验证：** `go build ./...`、`go vet` 通过；`go test ./internal/...` 全部通过（含 responses 包流式生命周期测试）。

### 2. 附带说明：Codex WebSocket 降级为 HTTPS（无需修复）

日志分析确认：WS→HTTPS 降级发生在 Codex 客户端侧 —— 代理发出的标题生成辅助请求（`codex_output_schema`、tools=0）触发 Codex 回落 HTTP 路径，14 次 WS 握手全部 101 成功，代理零责任，仅为观感问题，聊天不受影响。

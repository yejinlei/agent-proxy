# v0.2.144

## 变更：流式 `usage` 修复 —— 三条链路全断，逐一接通

一次**根因修复**。修的是流式响应的 token 统计量 —— 感诺上游（token.sensenova.cn）
在开启 `stream_options.include_usage` 时**确实返回 usage**，但代理在三条独立链路
上各断一处，导致下游拿到的永远是 `&{0 0 0 0 0}`。三处任一不修，usage 都是 0。

影响面是两条客户端路径：**Codex** 与 **Claude Code**。Kimi 走透传（入站
`chatcompletion` 归一化为 `openai`，感诺 `capabilities:["openai"]` 命中），
不经过任何翻译器，本来就拿得到 usage，本次零改动。

### 为什么这是「Codex 停住」的上游

Codex 的 `auto_compact_scope_tokens` 严格等于 `total_usage_tokens`
（实测 223/223 精确相等）。usage 恒 0 → 压缩阈值恒 0 → **自动压缩永不触发** →
对话历史无界增长 → 最终撞入站 body 上限。v0.2.143 已把上限从 1MiB 提到 16MiB，
但那是止血；这条链路不修，历史还是会一路长到顶。

### 三条链路

**链路 1 —— 代理根本没请求 usage**（`gateway.go` `buildCCRequest`）

v0.2.142 的实现按上游类型把 `streamOptions` 置 nil：

```go
if DetectUpstreamType(baseURL) == "sensenova" {
    streamOptions = nil
}
```

注释归因是「sensenova 拒绝该未知字段 → HTTP 400 Invalid request format」。**这个
归因是错的。** 用代理自己生成的真实请求体直连实测（同一体，只改这一处）：

| 请求 | 结果 |
|------|------|
| F1 原样 | HTTP 200，**无 usage** |
| F2 加 `stream_options` | HTTP 200，**有 usage** |
| F3 把 `system` 改成 `developer` | **HTTP 400** "inference request is invalid" |

真凶是 `role:"developer"`，不是 `stream_options`。感诺在所有开通模型上都接受
`stream_options`（glm-5.2 / sensenova-6.8 / deepseek-v4 / kimi-k3 均 200 + usage），
甚至容忍其内未知键。而代理从不发 developer —— 本函数第 1411 行硬编码 `"system"`，
`mapRoleToCC` 把 developer → system。

guard 声称的报错文本 "Invalid request format" 在感诺 **0 次复现**；实测 400 全是
"inference request is invalid"、"field TopP invalid, should be in (0, 1.0]"、
"field MaxTokens invalid, should be in [1, 131072]"、"field Messages invalid"、
"user content required" 这一族。

v0.2.142 因为这次误判一路置 nil 至今 —— 感诺上游永远拿不到 usage，这就是 Codex
历史无界增长的起点。

**链路 2 —— 代理丢弃了 usage 帧**（`quick.go` + `gateway.go`）

OpenAI 系（含感诺）把 `stream_options.include_usage` 的统计量放在**独立尾帧**
`choices:[]` + `usage`，位置在 finish 帧之后、`[DONE]` 之前。实测 4 轮，帧序固定：

```
choices[0].finish_reason:"length"  →  choices:[] + usage  →  data: [DONE]
```

finish → usage 间隔 **0ms**，usage → DONE 间隔 **0ms**。

但 `handleStreamRequest` 的 OpenAI 兼容分支写的是 `|| len(ccChunk.Choices) == 0`
直接 `continue` —— 把这帧静默丢掉。而它是**唯一**的来源。代理日志 13/13 条流全是
`usage=&{0 0 0 0 0}`。

**链路 3 —— 翻译器在尾帧到达前就收尾了**

Responses 与 Anthropic 翻译器的 `case "done"` 都拿到 finish 帧后直接发终态并
`return`，usage-only 尾帧永远收不到。

Anthropic 那边还多一个独立的协议违规：

```go
if usage != nil {          // 感诺的 finish 帧不带 usage → 恒 false
    ...发 message_stop...
}
fn(nil, true)              // 只发这个，不写任何 content
return
```

所以生产上 Claude Code **收得到 `message_delta`，收不到 `message_stop`** —— 违反
Anthropic 事件生命周期（`message_start → content_block_start → content_block_delta*
→ content_block_stop → message_delta → message_stop`），不只是 usage 为 0。

### 改动

**链路 1** — `gateway.go` `buildCCRequest`：`stream_options` 改为**无条件**注入。
quick.go 零改动，它调用的是同一个共享函数。

**链路 2** — `quick.go` + `gateway.go` 三处：`choices` 为空且 `usage` 非空时发一条
`Type:"usage"` 的 `InternalStreamEvent`，不再 `continue` 跳过。覆盖流式与
`handleStreamRequestAsNonStream` 聚合两条路径。

**链路 3** — 两个翻译器同一模式：

- `responses/translator.go`：`case "done"` 先做一次 `doneUsageWait`（100ms）有界
  等待取尾帧，再统一 `sendCompleted()`；新增 `case "usage"` 覆盖「上游不发 finish
  帧、只发 delta 后直接给 usage」的退化形态；`completedSent` 保证
  `response.completed` 只发一次。
- `anthropic/translator.go`：同样新增 `finishUsageWait`（100ms，与 Responses 同值）
  + `case "usage"`；channel 关闭分支也补做有界等待（`provider/openai.go` 的
  `CallStream` 在最后一帧数据之后可能立即 close channel，不等消费者读走）。
  新增 `sendTerminal` 幂等闭包（`terminalSent`），让 `case "done"` / channel 关闭 /
  `ctx.Done` 三条收尾路径都发**恰好一次** `content_block_stop → message_delta →
  message_stop`，且 `message_stop` **无条件**发送（修掉上面的协议违规）。

**100ms 不是等待机制，是收尾兜底。** 实测感诺 finish 帧与 usage 帧连发（间隔 0ms），
正常路径 0ms 内就取到尾帧。放大它会拉长每个正常流的收尾；放小到 0 则在上游 usage
帧稍有排队时漏发。上下文断连时 `ctx.Done` 与 events 在 select 内竞争，立即收尾、
不等尾帧。

### 顺带确认的两件不需要改的事

- **`~/.codex/models.json` 的 1M 上下文不用改。** `/v1/models` 返回 glm-5.2
  `context_length: 1048576`、`max_output_length: 131072`、`quantization: fp8`，
  text-only input，features `["tools","json_mode","reasoning"]`。配置是对的。
- **Kimi 走透传不需要修**，见上。透传路径把 SSE 行原样转发，不经过翻译器。
  注意一个前提：透传**不注入** `stream_options`，Kimi 能拿到 usage 是因为
  **它自己**在请求体里带了 `stream_options:{"include_usage":true}`。按
  「尽量透传、不阻止」的约定，代理没有兜底注入。

### 变更文件

- `internal/server/gateway.go`：`buildCCRequest` 无条件注入 `stream_options`
  （`@AI_GUARD: CC_STREAM_OPTIONS_SENSENOVA` 归因改正）+ `handleStreamRequest`
  usage-only 帧放行（`@AI_GUARD: CC_USAGE_ONLY_FRAME`）
- `internal/server/quick.go`：`handleStreamRequest` + `handleStreamRequestAsNonStream`
  usage-only 帧放行
- `internal/protocol/responses/translator.go`：`doneUsageWait` 常量 + `case "done"`
  有界等待 + `case "usage"` + `completedSent` 幂等
- `internal/protocol/anthropic/translator.go`：`finishUsageWait` 常量 + `sendTerminal`
  幂等闭包 + `case "done"` 有界等待 + `case "usage"` + channel 关闭分支补等
- `internal/protocol/responses/usage_frame_test.go`：**新增**，2 test
- `internal/protocol/anthropic/usage_frame_test.go`：**新增**，5 test
- `internal/server/root_cause_test.go`：删除「sensenova 必须省略 stream_options」的
  旧断言，改为 `TestCodex_StreamOptionsAlwaysInjected`（4 个上游均须注入）+
  `TestBuildCCRequest_NeverEmitsDeveloperRole`（锁定真凶不外泄）
- `main.go`：版本 `v0.2.143` → `v0.2.144`
- `CLAUDE.md` / `AGENTS.md`：guard 表 91 → 97 项，新增
  `CC_USAGE_ONLY_FRAME` / `CC_STREAM_OPTIONS_SENSENOVA` /
  `RESPONSES_USAGE_ONLY_FRAME` / `RESPONSES_COMPLETED_ONCE` /
  `ANTHROPIC_USAGE_ONLY_FRAME` / `ANTHROPIC_TERMINAL_ONCE`，保持逐字同步

### 验证

```bash
go build ./...                 # 干净
go test -count=1 ./...         # 全绿

go test ./internal/protocol/responses/ -run 'UsageOnly|NoFinishFrame' -v
    TestTranslateStream_UsageOnlyFrame              PASS
        END ... took=35ms usage=&{727 133 860 0 0}
    TestTranslateStream_NoFinishFrame_UsageOnly     PASS
        END ... took=0s    usage=&{100 20 120 0 0}

go test ./internal/protocol/anthropic/ -run AnthropicStream -v
    TestAnthropicStream_UsageOnlyFrame              PASS  # message_stop 带 727/133/860
    TestAnthropicStream_NoUsageTrailingFrame        PASS  # 无 usage 仍发 message_stop
    TestAnthropicStream_NoFinishFrame_UsageOnly     PASS
    TestAnthropicStream_ChannelClose_NoUsage        PASS  # 收尾于 0s
    TestAnthropicStream_MaxTokensStopReason         PASS  # length → max_tokens

go test ./internal/server/ -run 'StreamOptionsAlwaysInjected|NeverEmitsDeveloperRole' -v
    TestCodex_StreamOptionsAlwaysInjected           PASS  # 4 上游均带 include_usage
    TestBuildCCRequest_NeverEmitsDeveloperRole      PASS  # role 集合 = map[system user]
```

`TestTranslateStream_UsageOnlyFrame` 与 `TestAnthropicStream_UsageOnlyFrame` 在
旧代码上都会失败 —— 三条链路任一未修即红。`TestAnthropicStream_NoUsageTrailingFrame`
单独锁定「无 usage 也必须发 `message_stop`」这一协议违规的修复。

### 已知局限

- **Gemini 翻译器仍坏**（本轮有意不做）：`gemini/translator.go` 的
  `TranslateStream` 既没有 `case "usage"` 也不读 `done` 的 usage。若把 Gemini
  客户端连到这个感诺上游，usage 仍是 0。
- **透传路径不注入 `stream_options`**，见上。CC 透传路径拿得到 usage 完全依赖
  客户端自己带这个字段。
- **`CC_TOP_P_AND_MAX_TOKENS_FILTER` 的 max_tokens 上限写的是 65536，但 glm-5.2
  实际允许 [1, 131072]** —— guard 过严，会误丢 65537–131072 的合法值。未改。

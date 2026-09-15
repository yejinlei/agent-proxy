# v0.2.148

## 变更：撤回 v0.2.146 的翻译路径大请求降级（这条降级就是 Codex 丢响应的直接原因）

上一版给降级路由补了 60s 超时，但**补超时是把失败变有界，不是修好失败**——
每次降级 Codex 都拿不到任何合法事件，60s 只是让它等得更短就报错。
本版把降级整体撤回，并把降级指向的分支修成合法的事件序列，为将来留一条真正安全的路。

### 1. 现场：Codex 报 10054 / stream closed，但代理日志里响应是成功的

`agent-proxy-9091.log`（v0.2.146/v0.2.147，09-15 17:02–17:08）：

| 时间 | 下游体 | 模型 | 路由 | 上游结果 | Codex |
|---|---|---|---|---|---|
| 17:02:34 | 22762 | glm-5.2 | 原生流式 | 4.5s，usage 4412/184 | 正常 |
| 17:02:35 | 215999 | glm-5.2 | **降级** | 60.0s 超时 | 报错 |
| 17:02:40 | 28891 | glm-5.2 | 原生流式 | 3.2s，usage 5385/104 | 正常 |
| 17:03:36 | 215999 | glm-5.2 | **降级** | 60.0s 超时 | 报错 |
| 17:07:08 | 33769 | gpt-5.2 | 原生流式 | 6.2s，function_call，usage 7849/220 | 正常 |
| 17:07:16 | 269873 | gpt-5.2 | **降级** | 60.0s 超时 | 报错 |
| 17:07:36 | 22762 | glm-5.2 | 原生流式 | 2.6s，usage 4412/86 | 正常 |
| 17:07:39 | 29385 | glm-5.2 | 原生流式 | 9.7s，usage 5500/414 | 正常 |
| 17:08:17 | 269873 | gpt-5.2 | **降级** | 60.0s 超时 | 报错 |
| 17:08:29 | 269873 | gpt-5.2 | **降级** | **11.05s 成功** | 报错 |
| 17:08:29 | 269873 | gpt-5.2 | **降级** | 日志结束时仍在跑 | 报错 |

4 份 rollout、18 轮 `task_complete`，error 全是同一条：
`stream closed before response.completed` / `os error 10054`。

**决定性的一行是 17:08:29**：降级路径 11 秒就拿到了合法响应体（不是超时），
Codex 仍在 17:09:48 报 `stream closed before response.completed`。
**响应体合法，事件序列不合法。**

判别线很干净：**4 次原生流式 0 失败，5 次降级 5/5 失败**。
4 次原生流式全部返回真实非零 usage，说明 usage 那块（v0.2.144 的修复）是好的，
之前怀疑「加了获取 token 那块引入 BUG」不成立——它一直在工作。

### 2. 根因：降级发的帧没有 `type` 字段，Codex 全部静默丢弃

降级走 `handleNonStreamResponseAsSSE` → `writeNonStreamAsSSE`，其 OpenAI Responses 分支
原来发的是：

```json
{"id":"resp_...","object":"response.text_delta",
 "response":{"id":null,"model":"glm-5.2"},
 "output":[...],"usage":null}
```

没有 `type` 字段，没有 `created_at`，响应级 `object` 不是 `"response"`，
每个 item 的 `id` 全是 null。而 Codex 的 WS 路径对**整帧**做
`serde_json::from_str::<ResponsesStreamEvent>`、再靠 `type` 字段分发事件——
每条帧解析失败后走的是 `continue`（静默丢弃），不是报错。
于是整条流一个合法事件都进不了 Codex 的状态机，自然永远等不到 `response.completed`。

日志侧两个交叉验证：整份 `agent-proxy-9091.log` 里 `created_at` 出现 **0 次**、
`"object":"response"` 出现 **0 次**——降级路径产出的字节里根本没有
Codex 能识别的终态事件。

**为什么 v0.2.143 是好的**（已用 git 核实）：v0.2.143 的 `largeBodyThreshold` 判断只在
**透传**块里（调用 `handlePassthroughNonStreamAsSSE`）。`translationStreamRoute` 直到
commit `1ce7fb3`（v0.2.146）才出现。所以 v0.2.143 的翻译路径始终走原生流式。

**为什么这个分支在 v0.2.73 引入降级时是安全的**：那时透传路径只可能喂
Anthropic / Gemini / ChatCompletion 形状（Codex 走翻译），Responses 分支从未被执行。
v0.2.146 给了这个坏分支它的第一个真实调用方——bug 一直潜伏着，只是没人踩到。

### 3. 修复

**（a）撤回降级。** `translationStreamRoute` 函数整体删除，`if stream` 分支里不再有
按体积分流——直接调 `handleStreamRequest`。不留「恒真判断」这种死代码，
大请求只多打一行 `[route]` 日志供观测。

`gateway.go` 本就没有真降级（只有 log-only），现在两边一致了。
它那个 guard 的注释同时改写：原文档称「quick.go 有降级、gateway.go 缺降级、
要真降级需先补包装函数」，现在两边都没有，方向本身也已证明是错的。

`NONSTREAM_AS_SSE_STREAM_FLAG` 保留为防御性约束——透传路径仍调用
`handleNonStreamResponseAsSSE`，不是死代码。

**（b）把降级指向的分支修成合法事件序列。** 既然将来很可能再有人接回降级，
就让它接回去时不会再坏：`writeNonStreamAsSSE` 的 OpenAI Responses 分支改为发

```
response.created
→ 每个 output item 一对 response.output_item.added / response.output_item.done
→ response.completed
→ [DONE]
```

形状逐字段对齐 `responses/translator.go` 的 `sendCreated` / `sendCompleted`：
每条带 `event:` 前缀且与 `data.type` 一致；**不带响应级 `object`**；
`output_index` 按数组下标递增（不是常量）；`usage` 三键齐全为数字；
`finish_reason` 动态映射（output 里有 function_call item 时写 `function_call`）；
`incomplete_details.reason` 为 null；message item 补 `role` + 非空 `content` 数组；
function_call item 补 `name` / `arguments` / `call_id`。

v0.2.148 撤回降级后这个分支**已无调用方**，属于「修好地雷」而非「修好路径」。

### 4. 关于 v0.2.147 的结论

v0.2.147 的 release note 写过「降级路由本身是对的，它规避的是 v0.2.144 反复出现的 502」。
**那个判断错了。** 本次日志窗口（v0.2.146 → v0.2.148）里 `502` 出现 **0 次**
（`grep 502` 命中的只是心跳时长里的数字，如 `41.000791502s`），5 次大请求全部成功返回。
v0.2.144 的 502 在 usage 修复之后已不复现，v0.2.146 的降级是在修一个已经修好的问题，
代价是每次大请求都必然让 Codex 丢响应。

`nonStreamCallTimeout`（60s 上限）**保留**——上一版的实测结论「大请求两种模式都能完成，
且非流式明显更快」仍然有效，那个 60s 界是真实的防护。只是不该靠它来承担
「让 Codex 拿到合法事件」这个职责。

### 变更文件

- `internal/server/quick.go`：删除 `translationStreamRoute`、删除降级分支（改为始终走
  `handleStreamRequest`）；重写 `writeNonStreamAsSSE` 的 OpenAI Responses 分支为
  完整事件序列（新增 `NONSTREAM_A2S_RESPONSES_EVENTS` guard）
- `internal/server/gateway.go`：改写 `LARGE_BODY_SKIP_STREAM_TRANSLATION` 的归因注释
  （两边现均为 log-only）
- `internal/server/large_body_route_test.go`：删除 `TestTranslationStreamRoute`
  （函数已删），新增 `TestTranslationRoute_AlwaysStreams`（AST 锁流式分支恰好 1 次
  `handleStreamRequest`、0 次降级调用、0 次 `translationStreamRoute` 引用），
  保留 `TestLargeBodyThreshold_Value`
- `internal/server/nonstream_responses_events_test.go`：**新增**，
  `TestWriteNonStreamAsSSE_ResponsesEvents`
- `main.go`：版本 `v0.2.147` → `v0.2.148`
- `CLAUDE.md` / `AGENTS.md`：guard 表 102 → 103 项，新增 `NONSTREAM_A2S_RESPONSES_EVENTS`、
  重写 `LARGE_BODY_SKIP_STREAM_TRANSLATION`、更新 `NONSTREAM_AS_SSE_STREAM_FLAG`，
  标记数 245 → 249，保持逐字同步（含 CRLF）

### 验证

```bash
go build ./...            # 干净
go vet ./internal/server/ # 干净
go test -count=1 ./...    # 全绿
```

```
TestLargeBodyThreshold_Value                    PASS
TestTranslationRoute_AlwaysStreams/translation_流式分支      PASS
TestTranslationRoute_AlwaysStreams/translation_非流式分支   PASS
TestWriteNonStreamAsSSE_ResponsesEvents         PASS
```

**两处都做了负向验证**（改坏→测试如期 FAIL→恢复）：

- 把 `LARGE_BODY_SKIP_STREAM_TRANSLATION` 的降级接回去 → `TestTranslationRoute_AlwaysStreams`
  FAIL（「translation 流式分支出现了 1 次非流式→SSE 调用」）
- 把 `output_index` 改回常量 `len(items)-1` → FAIL（「第 1 个 added 的 output_index = 1，want 0」）
- 把响应级 `object` 加回去 → FAIL（「response 载荷里带了 object 字段」）

`TestTranslationRoute_AlwaysStreams` 与 `header_forward_test.go` 同一思路，用 AST 而非
反射：唯一会回归的方式就是「把分支改回 `if len(downstreamReq) > threshold` 再降级」，
编译期与 `go vet` 对此零提示，唯一的后果是下一次大请求 Codex 丢响应。
测的正是「这个块里出现了哪些调用」。

`TestWriteNonStreamAsSSE_ResponsesEvents` 直接调用生产函数、拆帧逐条断言：
每帧必须有 `event:` 前缀且与 `data.type` 一致；禁止出现
`"object":"response.text_delta"` 与 `created_at`；禁止出现会让 Codex 直接
`return Err` 的 `response.failed` / `response.incomplete`；
`response.created` 必须存在、`response.completed` 必须存在且晚于所有 added/done；
added 与 done 数量相等且 added 在前；`[DONE]` 必须是最后一帧；
`output_index` 必须 0,1,2… 递增；`response.completed` 载荷的
`id` / `status` / `usage` 三键 / `finish_reason` / `output` 逐项检查。

### 已知局限

- `writeNonStreamAsSSE` 的 Responses 分支目前无调用方，只能靠测试守住形状。
  如果将来重新启用降级，请先跑该测试确认输出形状。
- gateway.go 翻译路径仍是 log-only，行为与 quick.go 一致但无降级能力，属独立工作量。
- 上游偶发挂死本身未被解决，仍由 `nonStreamCallTimeout`（60s，非流式）与
  `upstreamStallTimeout`（60s，流式 idle）分别兜底。

# v0.2.142

## 变更：`tool_search` 工具双向桥接，补齐 Codex 的 deferred 工具通路

一次**功能补齐**。修的是工具面缺失 —— Codex 送来的 `tool_search` 被代理丢弃，
导致所有 deferred MCP 工具不可达。

### 问题

v0.2.142 之前，`buildCCRequest` 每轮固定丢弃 2 个工具，日志 `2 tool(s) have no CC
equivalent` 出现 12/12 轮。被丢弃的是 `[tool_search, web_search]`。

`tool_search` 的丢弃不是无害的。`tool_namespaces_info.rs:80` 的 deferred 门是：

```
deferred = exposure.is_deferred() && native_tool_search_visible && (...)
```

`native_tool_search_visible` 取决于 `tools[]` 里有没有 `type:"tool_search"`。
代理把它丢了 → 用户配置的 `mcp_servers.*`（本机的 `codegraph`、`zvec_grep`）
**全部 deferred 工具不可达**，只有 3 个 MCP meta 工具能过。而 `tool_search` 自己的
description 明确说要用它替代 `list_mcp_resources` —— 门和钥匙被一起扣掉了。

### 为什么必须双向

只合成入站（让模型看见 `tool_search`）是不够的，会造成**静默致命**：

- Codex 侧 `router.rs:244`：`ResponseItem::ToolSearchCall` → `ToolPayload::ToolSearch`；
  `ResponseItem::FunctionCall` → `ToolPayload::Function`，**无条件**，没有按名字回退。
- `handlers/tool_search.rs:200-207`：`handle_call` 严格匹配 `ToolPayload::ToolSearch`；
  收到 `ToolPayload::Function` 直接 `FunctionCallError::Fatal("tool_search handler
  received unsupported payload")`。

所以：只合成入站 = 模型调用后代理回 `function_call` = Codex Fatal，整轮挂掉且没有任何
提示。反过来只改出站 = 模型根本看不见这个工具，改写了个寂寞。

### 两处改动

**入站合成** — `internal/server/gateway.go` `buildCCRequest`：

`type:"tool_search"` → `{"type":"function","function":{"name":"tool_search",
"description":<原文>, "parameters":<原文>}}`。

工具名是字面量 `"tool_search"`，与 `codex-rs/tools/src/tool_discovery.rs:6` 的
`TOOL_SEARCH_TOOL_NAME` 常量逐字节一致；handler 注册用的是 `ToolName::plain`，不带
namespace。description / parameters 原样透传，代理只转形状不转语义。

与 custom（`exec_<原名>`）和 namespace（`<ns>_<toolname>`）不同，**这里不需要
ctx/名表通道**：合成名等于 Codex 原名，出站按名字相等即可判定。

**出站改写** — `internal/protocol/responses/translator.go`（流式 + 非流式两条）：

`function_call(name="tool_search")` →

```json
{"type":"tool_search_call","id":...,"call_id":...,"execution":"client",
 "arguments":{"query":"...","limit":N}}
```

形状依据 `protocol/src/models.rs:1081` `ResponseItem::ToolSearchCall`：`arguments`
是 **JSON 对象**（不是 `function_call` 的字符串），`execution` 必填。参数集是
`SearchToolCallParams { query, limit }`，只有两个字段。

quick.go **零改动** —— 它调用的是同一个共享 `buildCCRequest`。

### `web_search` 保持丢弃

`web_search` 的 `ToolSpec` 没有 `execution` 字段（`tool_spec.rs:44-54`）—— 是
server-side/hosted 工具，Codex 侧无本地 executor，CC 侧也没有。合成了就是造幻影：
模型看见工具、尝试调用，但没有任何一方执行。这个丢弃是**正确的**，不是遗漏。

### 校验与回退策略

出站 arguments 必须合法，但两条规则不同：

| 场景 | 处理 |
|------|------|
| `query` 缺失 / 非字符串 / 空白 | **保留 `function_call`** + 打日志 |
| `limit` 负数 / 非整数 / 超限 | **丢弃该字段**，保留 `query` |
| 多余字段（如 `bogus`） | 剥离 |

`limit` 在 Codex 侧是 `Option<usize>`，缺失时回落到
`TOOL_SEARCH_DEFAULT_LIMIT` —— 这是**合法的降级**。所以 `limit` 坏只是丢掉可选值。

`query` 是必填（`tool_search.rs:213-219` 空 query → `RespondToModel("query must not
be empty")`，`:217` 还会 `trim`）。而这里的关键权衡是：一个**非法的**
`tool_search_call` 永远比保留 `function_call` 更糟 —— Codex 一定拒绝前者，后者
至少让模型看见自己传错了什么。所以 `query` 坏就退回 `function_call`，不做降级。

### added/done 类型必须一致（且必须按**完整**参数判定）

流式的 `output_item.added` 和 `output_item.done` 携带同一个 `output_index`，
两者的 `type` 必须一致。

Codex 侧证据（`codex-rs/core/src/session/turn.rs:2619` + `stream_events_utils.rs`）：
两个事件被**各自独立**解析成 `ResponseItem`。`OutputItemAdded` 走
`handle_non_tool_response_item`（`:407`，对 `Message`/`Reasoning`/`WebSearchCall`
建展示用 turn item，其余一律返回 `None` —— 包括 `ToolSearchCall` 和
`FunctionCall`）；`OutputItemDone` 才走 `ToolRouter::build_tool_call`
（`stream_events_utils.rs:307`）派发工具。所以 added 与 done 类型不一致**不会**
报协议错误，但会让这个 output_index 的注册落空、done 变成孤儿 item。

初版实现就是栽在这里：`SearchValid` 在**首次见到名字时**按当时的 arguments 锁定。
但真实上游几乎总是把 tool_call 参数拆成多片（首片带 name、参数逐片追加），
首片要么参数为空、要么是 JSON 前缀（`{"query":` 单独 Unmarshal 必失败）。
初版因此把**合法调用永久降级成 `function_call`** → Codex 以
`ToolPayload::Function` 派发到 `ToolSearchHandler` → Fatal，且没有任何日志线索。

正确做法：

1. `SearchValid` 创建 fc 时钉 `false`（此时参数必然为空 → 必然非法）。
2. added **必须延后**：`sendOutputItemAddedEarly` 对 `fc.Search` 的 fc 直接跳过，
   普通 function_call 仍在首个 delta 即时发（客户端可即时渲染）。
3. `closeFuncCall` 先用**完整**参数重判 `SearchValid`，再按需补发 added，紧接发 done。
   顺序是「重判 → 补发 added → 发 done」，三者读同一个结论，type 才一致。
4. `output_item.added` 的 item 只用于注册，`arguments` 字段**不写**（Codex 侧对
   `ToolSearchCall` 的 added 直接返回 `None`，不承载参数）；`output_item.done` 与
   `response.completed` 里的 `arguments` 必须始终存在且是对象。写 `null` 会让
   `SearchToolCallParams.query` 缺失 → `RespondToModel`，白白浪费一整轮。

另外 `tool_search_call` 不发 `function_call_arguments.*` 事件（与 custom 同约定）——
那些事件只绑定 function_call item。`finalizeItems` 的 `response.completed.output[]`
也必须与 done 逐字段一致：空参数只在**普通 function_call** 上兜底成 `"{}"`
（Codex 的 `FunctionCall.arguments` 是 String，空串解不成 Value），custom 与
tool_search 保持原样。

### 变更文件

- `internal/protocol/responses/tool_search.go`：**新增**，`ToolSearchParams`（入站
  形状解析）+ `IsToolSearchArgsValid`（出站 arguments 校验/清洗），两个 `@AI_GUARD`
- `internal/server/gateway.go`：`buildCCRequest` 新增 `tool_search` 合成分支 +
  观测日志，`@AI_GUARD: CC_TOOL_SEARCH_SYNTHESIS`（4 段注释：3 CONSTRAINT + 2 REASON）
- `internal/protocol/responses/translator.go`：流式 `funcCallState` 增加
  `Search`/`SearchValid`、`getFC` 两处钉住判定、`customFCItem` 加
  `tool_search_call` 分支、`closeFuncCall` 不兜底 `{}`、`sendFuncArgsDelta` 跳过、
  `TranslateResponse` 加非流式分支；`@AI_GUARD: RESPONSES_TOOL_SEARCH_BRIDGE` /
  `RESPONSES_TOOL_SEARCH_VALIDITY`
- `internal/protocol/responses/tool_search_test.go`：**新增**，5 test / 12 子用例
- `internal/protocol/responses/tool_search_bridge_test.go`：**新增**，7 test
  （含 namespace 前缀不被误判、非法 arguments 回退、空 arguments、args 清洗）
- `internal/server/cc_tool_search_test.go`：**新增**，4 test（含 web_search 不得
  合成的负向断言）
- `main.go`：版本 `v0.2.141` → `v0.2.142`
- `CLAUDE.md` / `AGENTS.md`：guard 表 85 → 87 项，新增
  `CC_TOOL_SEARCH_SYNTHESIS` + `RESPONSES_TOOL_SEARCH_BRIDGE`，保持逐字同步

### 验证

```bash
go build ./...                 # 干净
go test ./...                  # 全绿
go test ./internal/protocol/responses/ -run ToolSearch -v
    TestToolSearchParams_PassesRealCodexShape        PASS
    TestToolSearchParams_RejectsEmptyDescription     PASS
    TestIsToolSearchArgsValid_AcceptsAndCleans       PASS
    TestIsToolSearchArgsValid_RejectsMalformed       PASS
    TestIsToolSearchArgsValid_DropsBadLimitKeepsQuery PASS
    TestTranslateStream_ToolSearchCall               PASS
    TestTranslateStream_ToolSearchArgsCleaned        PASS
    TestTranslateStream_ToolSearchInvalidArgsKeepsFunctionCall PASS
    TestTranslateStream_ToolSearchEmptyArgs          PASS
    TestTranslateResponse_ToolSearchCall             PASS
    TestTranslateResponse_ToolSearchInvalidArgsKeepsFunctionCall PASS
    TestTranslateResponse_ToolSearchNotConfusedWithNamespace PASS
go test ./internal/server/ -run ToolSearch -v
    TestBuildCCRequest_ToolSearchSynthesized         PASS
    TestBuildCCRequest_ToolSearchSkippedWhenRawEmpty PASS
    TestBuildCCRequest_ToolSearchSkippedWhenDescriptionMissing PASS
    TestBuildCCRequest_OtherUnrecognisedTypesStillDropped PASS
```

### 已知局限

- **非 Codex 客户端的名字碰撞**：任何走 Responses 协议、但恰好有一个函数就叫
  `tool_search` 的客户端，它的调用会被改写成 `tool_search_call` item。判定是精确
  名字相等（不是前缀），但仍是名字启发式。概率低，不加防护。
- **`web_search` 仍然丢弃**，见上，是有意为之。
- **模型是否会真的用它**：`tool_search` 送达只保证门开了。`buildCCRequest` 的
  观测日志 + `TranslateStream` END 行的 `fcs=[...]` 可以 grep 出「工具送达了但
  模型 0 次调用」—— 那是模型/上游的行为问题，不是代理能修的。

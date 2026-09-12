# v0.2.139

## 变更：入站 `reasoning` 参数的丢弃观测

纯可观测性变更，**不涉及任何协议形状改动**。

### 背景

v0.2.138 生产日志三轮 END 行的 `reasoning_chars` 分别是 440 / 393 / 511——模型全程都在思考。
但入站带 `"reasoning":{"effort":"medium","summary":"auto"}`，上游 CC 请求体只有
`messages / model / stream / tools` 四个顶层字段，**没有任何对应字段**。

中间这段是盲区：入站能 grep 到字段名（`top_fields=[...]` 里有 `reasoning`），
但值既没记录也没映射，只能翻 WS frame 原文逐字符比对才能确认"是代理丢弃的"。
这跟 v0.2.132 之前「连丢了哪些工具都看不到，只能靠猜」是同一类故障。

### 现在的行为

每条 `TranslateRequest meta:` 之后多一行：

| 入站状态 | 日志 |
|---------|------|
| 带 `reasoning` | `reasoning=effort="medium" summary="auto" (not forwarded to CC)` |
| 未带 | `reasoning=absent` |

`absent` 与 `effort=…` 是两种不同状态，必须能 grep 区分——不能两种都打成空串，
否则"客户端没发"和"代理丢了"无法分辨。

固定后缀 ` (not forwarded to CC)` 是为了让日志自证：看到值不等于看到了传递，
读到后缀才知道这是丢弃点。

### 实现

- `codexReasoningMeta(raw)`：把 `reasoning` 解成 `key=value` 扁平串。
  不建模结构（`summary` 在 Codex 侧可能是数组或字符串两种形态），
  只压 JSON 原文，避免对 Codex 版本演进过敏。非对象形态与非法 JSON 原样返回。
- `codexReasoningArg(raw)`：统一缺失 → `absent`、有值 → `<值> (not forwarded to CC)`。

### 为什么只观测不传递

`reasoning.summary` 是 OpenAI 专有能力（控制思考内容的可见性），
CC 侧对应字段名是 `reasoning_effort` 且无 summary。直接映射进 CC 请求体
有 400 风险（参考 `CC_STREAM_OPTIONS_SENSENOVA`：sensenova 对 CC 端点不认
`stream_options` 直接 400）。与 `RESPONSES_INBOUND_META` /
`RESPONSES_CODEX_SANDBOX_META` 同一条护栏：**只打日志，禁止据此改变出站行为**。

### 变更文件

- `main.go`：版本 `v0.2.138` → `v0.2.139`
- `internal/protocol/responses/translator.go`：`TranslateRequest` 新增
  `reasoning=…` 观测行 + `codexReasoningMeta` / `codexReasoningArg` helper
  + `@AI_GUARD: RESPONSES_REASONING_META` 观测护栏
- `internal/protocol/responses/codex_meta_test.go`：`TestCodexReasoningMeta`
  （5 个形态：真实形状 / summary 数组 / 标量 / 空 / 非法 JSON）
  + `TestTranslateRequestReasoningMetaLog`（生产路径落日志，含 absent 分支）
- `CLAUDE.md` / `AGENTS.md`：guard 表新增 `RESPONSES_REASONING_META`
  （79 项 / 193 处标记），保持逐字同步

`internal/protocol/anthropic/` 未改动。`TranslateRequest` 是入站翻译器方法，
与 quick.go / gateway.go 双模式同步无关（两条模式共用同一个翻译器实例）。

## Codex 侧交叉验证（`F:\src\codex`，本次只读未改）

上一版 v0.2.138 结论是「工具面缺失导致模型没有落盘能力，代理无改动可做」。
本次把门控链彻底走完，确认该结论成立，并补上三个此前未落地的证据。

### 1. `apply_patch` 缺失的根因：模型目录，不是配置

```rust
// codex-rs/core/src/tools/spec_plan.rs:1255
if environment_mode.has_environment() && context.model_info.apply_patch_tool_type.is_some() {
    let include_environment_id = matches!(environment_mode, ToolEnvironmentMode::Multiple);
    registry.add(ApplyPatchHandler::new(include_environment_id));
}
```

`apply_patch_tool_type` 是 `Option<ApplyPatchToolType>`，只在
`codex-rs/protocol/src/openai_models.rs:439` 定义（`ModelInfo` 字段），
**仓库里唯一的非 None 赋值点在一个测试文件里**
（`core/tests/suite/auto_review.rs:639`）：

```rust
// codex-rs/models-manager/src/model_info.rs:168
apply_patch_tool_type: None,
```

模型目录来自 `codex-rs/models-manager/src/lib.rs:15` 的
`include_str!("../models.json")` 内嵌目录 + `manager.rs` 的远程 `list_models`。
`vmodel` 是代理的模型别名（`--aliases` 层解析成 `sensenova-6.8-flash-lite`），
不在 Codex 的模型目录里 → 走默认 `ModelInfo` → `apply_patch_tool_type: None`
→ `ApplyPatchHandler` 不注册。

**结论：代理无法通过改出站工具集补上 `apply_patch`。** 工具表是 Codex 本地
按自己的模型目录构造的，代理收到的已经是成品。要让它出现，只能改 Codex
的模型目录（仓库内 JSON 或托管配置），代理无能为力。

### 2. `web_search` 的丢弃符合 Codex 设计

```rust
// codex-rs/core/src/tools/spec_plan.rs:630
model_info.supports_search_tool && namespace_tools_enabled(turn_context)
```

`search_tool_enabled` 需要模型目录的 `supports_search_tool` 能力位为真。
自定义 provider 走 `ProviderCapabilities::default()`（`namespace_tools=true`），
但 `supports_search_tool` 由模型目录决定 → 默认模型为 false → 不发 `web_search`。
代理丢它并打日志是正确的，且 v0.2.138 起可见。

### 3. `update_plan` 缺失是用户配置，不是能力缺失

```rust
// codex-rs/core/src/config/mod.rs:2672
fn resolve_update_plan_enabled(config_toml: &ConfigToml) -> bool {
    config_toml.tools.as_ref()
        .and_then(|tools| tools.update_plan.as_ref())
        .is_some_and(|config| config.enabled)
}
```

默认 false（`.is_some_and` 语义：无配置项 → false）。用户在 `~/.codex/config.toml`
加 `[tools.update_plan] enabled = true` 即可打开，与代理无关。

### 4. namespace 命名规则

```rust
// codex-rs/protocol/src/tool_name.rs:7
pub const DEFAULT_FUNCTION_NAMESPACE: &str = "functions";
```

```rust
// codex-rs/core/src/tools/handlers/tool_search.rs:470
callable_namespace: format!("mcp__{server_name}")
```

MCP server 工具展平成 `mcp__<server>_<tool>`（所以 `mcp__codegraph_codegraph_explore`
的下划线是命名空间名的一部分，不能按前缀反推——这是 v0.2.137 已修的问题）。
Responses Lite 路径会把顶层 function 包进合成命名空间 `"functions"`
（`client.rs:779` → `create_tools_json_for_responses_lite`），
本环境的上游请求走顶层 `tools[]`（`tools=17` 里 `exec_command` 是平铺的），
未触发；记录在此供模型切换时参考。

### 5. 三轮 `tools` 差异是 Codex 的设计，不是代理漏送

- **轮 1**（`tools=14`，`text.format` 缺失）：真实 agent 轮。
- **轮 2**（`tools=0`）：纯文本输出轮。
- **轮 3**（`tools=0`，`text.format=type=json_schema name="codex_output_schema"`，
  `sandbox_mode=read-only`）：结构化输出轮。

`codex_output_schema` 只在一个地方产生：

```rust
// codex-rs/codex-api/src/common.rs:389
format: output_schema.as_ref().map(|schema| TextFormat {
    r#type: TextFormatType::JsonSchema,
    strict: output_schema_strict,
    schema: schema.clone(),
    name: "codex_output_schema".to_string(),
}),
```

由 `create_text_param_for_request` 在 `output_schema` 存在时注入。
它是**任务完成后的结构化总结/回收请求**，只带 3 条 input、`read-only` sandbox、
`tool_choice=auto`，本身就不带任何工具面。

所以 `ns_tools` 三轮分别是 7 / 0 / 0，完全对得上——
`ns_tools=7 ns_calls=0` 只发生在轮 1，那一轮模型主动收口吐散文
（`finish_reason=stop`、`func_calls=0`、`text_chars=66`），
不是代理漏送工具。

### 6. `x-codex-turn-metadata` 的完整字段清单

`codex-rs/core/src/responses_metadata.rs:509` `CodexTurnMetadataPayload`：

`installation_id` `session_id` `thread_id` `agent_name` `turn_id`
`window_id` `window_number` `context_window_id` `request_kind`
`forked_from_thread_id` `forked_from_ordinal_exclusive`
`parent_thread_id` `parent_turn_id` `root_turn_id` `subagent_kind`
`thread_source` `turn_trigger` `sandbox`（+ 后续字段）

代理现在只观测其中 `sandbox_mode` / `approval_policy` 两个。
`request_kind` / `turn_trigger` / `thread_source` 理论上可以进一步区分
"真实 agent 轮 vs 结构化输出轮"，但当前日志已经能用
`text_format` + `tools` 数 + `input_items` 数判断，不值得为纯观测再加解析。
`thread_id` / `session_id` 属于客户端标识，不适合进生产日志。

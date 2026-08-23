## v0.2.104

### 修复

**`gpt-4o-mini` / `gpt-4o` / `o3-mini` 不在默认别名列表中导致上游 400**

**根因：** `DefaultAliases()` 中缺少 `gpt-4o`、`gpt-4o-mini`、`o3-mini` 三个 OpenAI 模型别名。当客户端使用这些模型名时：

```
DefaultAliases().Resolve("gpt-4o-mini") = ("gpt-4o-mini", false)  ← 未命中
  → resolveAlias() 返回 hit=false，直接透传原值
  → Sensenova 收到 model="gpt-4o-mini" → 不认此模型 → HTTP 400
```

对比已存在的别名（如 `gpt-4`）：
```
DefaultAliases().Resolve("gpt-4") = ("gpt-4", true)  ← 自映射命中
  → 触发 _default_=@default 分支
  → ensureModels() 调用上游 /v1/models
  → 动态解析为 "sensenova-6.7-flash-lite"
  → HTTP 200
```

**修复：** 在 `internal/db/aliasfile.go` 的 `DefaultAliases()` 中补入 `gpt-4o`、`gpt-4o-mini`、`o3-mini` 三个别名，使其自映射并触发 `_default_` 动态解析到上游首个可用模型。

**附带发现（未修）：**
- `response_format:{type:"text"}` 会泄露到 CC 上游请求（CC 只接受 `json_object`/`json_schema`），需后续评估
- 非流式请求也带 `stream_options:{include_usage:true}`，部分上游可能拒绝，需后续评估

**测试：** 新增 `internal/server/root_cause_test.go`，包含：
- `TestRootCause_ModelAliasMissing` — 验证 3 个缺失别名已触发 `@default`
- `TestRootCause_LeakageFields` — 记录附带发现的字段泄露
- `TestEndToEnd_ModelAliasReplacement` — 端到端验证 `gpt-4o-mini → sensenova-6.7-flash-lite`
- `TestDeveloperToSystem` — 回归验证 developer→system 映射

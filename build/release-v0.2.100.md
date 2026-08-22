## v0.2.100

### 修复

**Responses 流式错误终态导致重复 `[DONE]`，Codex 提前关流**

**根因：** `responses/translator.go` `TranslateStream` 的 `case "error":` 分支在上游返回错误时发出 `response.failed` + `[DONE]` 后用了 **`continue`** 而非 `return`，导致 loop 重入后遇到关闭的 events channel，再次触发 `sendCompleted()` 发出完整的 `output_text.done → output_item.done → response.completed → [DONE]` 序列。

结果：SSE 事件序列中 `[DONE]` 出现在 `response.completed` 之前：
```
created → added → [DONE]   ← 提前发！
  → output_text.done → item.done → completed → [DONE]
```
Codex 在第一个 `[DONE]` 即关闭流，后续的 `response.completed` 无法送达。

**修复：**
1. `case "error":` 的 `continue` → `return`（错误是终态，不应重入 loop）
2. 补上游错误体诊断日志 `[CODEX-DEBUG] TranslateStream upstream error: status=... type=... message=...`，便于下次定位 Sensenova 返回的具体错误

**影响范围：** 仅限 `responses/translator.go` 的 `TranslateStream` 错误处理分支。正常响应路径（`start`/`delta`/`done`）不受影响。

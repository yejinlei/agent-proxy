## v0.2.39

### 修复
- **Thinking block delta 字段名错误（`handlePassthroughNonStreamAsSSE`）**：v0.2.38 修复了 delta 类型字符串（`thinking_delta`），但 delta 内字段名仍使用 `text` 而非 Anthropic 协议要求的 `thinking`。Anthropic Messages 协议中 thinking 块的 delta 格式为 `{"type":"thinking_delta","thinking":"..."}`，text 块的 delta 格式为 `{"type":"text_delta","text":"..."}`。现按 block 类型分别设置正确的字段名。
- **非 text/thinking 块静默发送空 delta 事件**：`content` 数组中的 `tool_use`、`image`、`tool_result` 等非文本块此前会发送 `{"type":"text_delta","text":""}`（空 delta），现直接跳过不发送。

### 根因
- v0.2.38 thinking 修复只覆盖了 delta 类型字符串，遗漏了 Anthropic 协议中不同 block 类型的 delta 字段名不同（thinking → `thinking`，text → `text`）

---


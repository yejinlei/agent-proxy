## v0.2.99

### 修复

**Codex `Invalid request form` — tool_result 内容块被静默跳过**

**根因：** `responses/translator.go` 的 `inputToMessages()` 函数在处理 `input[]` 中的内容块时，switch 只覆盖 `"output_text"` / `"input_text"` / `"input_image"` 三种类型，对 `"tool_result"` 类型的块直接跳过（`continue`）。当 Codex 发起带工具调用历史的重试请求时，工具结果内容被丢弃，导致消息退化为 `{"role":"user","content":null}`，上游 sensenova 拒绝，报 `Invalid request form`。

**修复：**
1. 在内容块 switch 中新增 `case "tool_result":` 分支：
   - 优先取 `tool_call_id` 字段，回退到 `item.ToolCallID`
   - 内容支持两种格式：纯字符串、数组（取每个子块 `text` 拼接）
   - 转为 `InternalMessage{Role:"tool", ToolCallID, Content}`
2. 消息末尾增加空消息过滤：当 `text == "" && contentBlocks == 0 && ToolCalls == 0` 时 `continue`，避免生成 `content:null` 的空消息

**影响范围：** 仅限 Responses 协议的入站翻译入口（`ResponsesTranslator.TranslateRequest()` → `inputToMessages()`）。CC / Anthropic / Gemini 三条入站路径完全不受影响。

### 版本

- `main.go` `version` → `v0.2.99`
- `build.ps1` `$VERSION` → `v0.2.99`
- 6 平台交叉编译通过（windows/linux/darwin × amd64/arm64）

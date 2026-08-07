# v0.2.7 — 命令参数校验

**`run` 和 `db add` 命令现对参数进行白名单校验，写错选项会给出明确错误提示，而非静默忽略。**

## Bug 修复

- **`--model` 不再被静默忽略** — `run` 命令仅接受 `--mode`，误写 `--model` 现在返回 `❌ 未知参数: --model` 并退出（此前 `--model simple` 被忽略，服务静默启动到复杂模式）
- **`--mode` 值校验** — 仅接受 `simple` / `complex`，传入其他值返回错误
- **`--key` 复杂模式警告** — `--key` 仅在快速模式下有效，复杂模式下给出 `⚠️` 警告而非静默吞掉
- **`db add --type` 校验** — 仅接受 `openai` / `anthropic` / `gemini`，传入其他值返回错误

## 测试

```bash
./agent-proxy run --model simple     # → ❌ 未知参数: --model
./agent-proxy run --mode badvalue    # → ❌ --mode 无效
./agent-proxy db add --type invalid  # → ❌ --type 无效
```

## 发布说明

- 修复项均由 7576ec3 引入，自 v0.2.6 以来的唯一变更

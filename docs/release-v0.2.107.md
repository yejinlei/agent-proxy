# v0.2.107

## 新增

### 1. 上游请求顶层字段清单诊断（翻译路径）

**问题：** v0.2.106 排查 Codex→Sensenova HTTP 400 时，关键诊断信息（`tools` / `stream_options` 等 offending field）位于 60KB+ body 的**尾部**，而 `formatJSON` 只保留头部 20KB，导致日志中恰好截掉了最需要的字段，只能靠源码反推根因。

**修复：**

| 改动 | 文件 | 说明 |
|------|------|------|
| `bodyKeyInventory` 顶层字段清单 | `internal/server/quick.go` | 在上游调用**之前**打印每个顶层字段的名称、类型（scalar / `{`-container / `[`-container）、字节长度与截断值，稳定排序便于跨请求 diff |
| 不受 `-v`/`-vv` 门控 | `internal/server/quick.go` | 诊断必须可见，否则上游 400 即时返回时 success-path 日志根本不会被调用 |
| 不受 20KB 截断影响 | `internal/server/quick.go` | 只看字段名，不看完整 body |
| `formatJSON` 尾部输出 | `internal/server/quick.go` | body > 20KB 时同时输出**尾部 8KB**；头部 20KB + 尾部 8KB 有重叠时完整输出 |

翻译路径 `nil downstreamReq` 时额外打印 `[diag]` 告警，提示请求将无法发送。

## 修复

### 2. 文档准确性（三项）

| 修复 | 文件 | 说明 |
|------|------|------|
| 移除已废弃的 `--stream-mode` 引用 | `docs/DESIGN.md` | 该参数自 v0.2.60 起已删除，但设计文档仍有 20 处引用，含 2 个 mermaid 子图、路由决策树节点、4 个按模式划分的 handler 表。已按当前真实路由逻辑（请求体 `stream` 字段 + `streamPrefer` 探测 + 100KB 阈值）重写 |
| 标注不可达场景 | `docs/DESIGN.md` | 9 张消息转换图重新标注可达性：仅场景 1/2/6/7 可达。场景 3/4/5/8/9 描述的「入站流式取值 ≠ 上游流式取值」组合在移除 `--stream-mode` 后已不可达 |
| 修正 `-v`/`-vv` 模式差异 | `README.md` | 补注「`-v` 仅快速模式生效，复杂模式需用 `-vv`」——复杂模式全部 40 处日志点均为 `verboseLevel >= 2` |

**注：** `docs/draw_flow.py` 与 7 张生成的 SVG 仍含 `--stream-mode` 标注，DESIGN.md 已在该章节顶部加警告说明，未重新生成。

### 3. API Key 泄露清理

| 修复 | 文件 | 说明 |
|------|------|------|
| 示例 Key 替换为占位符 | `MANUAL.md` | 全文 scrub，包括 `db query` 输出示例中的部分残留（`sk-b9****2mFKf` → `sk-abc***xyz`） |

**⚠️ 该 Key 曾真实提交至仓库历史，需要在上游吊销并重新签发，仅清理文件不构成补救。**

### 4. README 命令清单修正

`README.md` 命令组列出 `completion`（生成 shell 自动补全脚本），该命令在 `main.go` 中不存在。已替换为实际存在的 `version`。

## 验证

- `go build ./...` → exit 0
- `go test -count=1 ./internal/server/` → PASS（23.4s）

**⚠️ 部署注意：** 代码修复必须配合进程重启才能生效。部署后确认 `agent-proxy --version` 输出 v0.2.107，kill 旧进程再重启，跑一次实际客户端请求确认端到端。

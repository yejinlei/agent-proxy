## v0.2.95

### 新功能

**1. OpenAIPath 端到端透传（Google Gemini 双协议路由）**

- `internal/provider/openai.go`：新增 `NewOpenAIClientWithPath(name, baseURL, timeout, pathPrefix)`，可指定非标准前缀（如 `/v1beta/openai`），空值回退到 `/v1`
- `internal/server/quick.go`：`QuickGateway` 新增 `openAIPath` 字段，透传到 `getProvider("openai")` 的 `NewOpenAIClientWithPath`
- `internal/server/gateway.go`：Provider 注册表改用 `NewOpenAIClientWithPath(pc.OpenAIPath)`
- `internal/config/config.go`：`ProviderConfig` 新增可选 `openai_path` 字段
- `internal/db/db.go`：`Add/Update/scan` 持久化 `openai_path`，自动执行 `ALTER TABLE` 迁移
- `cmd/cli.go`：add/check/update 时把嗅探到的 OpenAIPath 写入 `ProxyRecord`
- `main.go`：将 `record.OpenAIPath` 传入 `NewQuickGateway`
- 目的：Google Gemini 用 `/v1beta/openai` 前缀而非 `/v1`，此前硬编码为 `/v1` 导致双协议路径下 Gemini 流量错路由

**2. Google Gemini Bearer 401 → `?key=` 回退**

Google Gemini API 不接受 `Authorization: Bearer` 头（仅 `?key=` 查询参数）
- 嗅探：Bearer 401 时重试 `GET /v1/models?key=<key>`，否则报 "未检测到任何大模型"
- 调用：Bearer 401 时重试 `POST <gemini-model>:generateContent?key=<key>`
- 顺手修复：硬编码的 `gemini-pro`（已废弃）改为从 OpenAI 模型列表嗅探首个 `gemini-` 模型

### 文档（GitHub Pages 站点上线）

- 上线 `docs/index.html` + 3 个 Archify 交互图 + `md-viewer.html`（Mermaid 10.9.0 + highlight.js + 深色模式）
- 新增 `ARCHITECTURE.md`（系统架构）、`REQUEST_LIFECYCLE.md`（请求生命周期）、`PROTOCOL_COMPARISON.md`（协议对比）、`OPENAIPATH_CHAIN.md`（OpenAIPath 链路）
- `.md` 卡片链接统一走 `md-viewer.html?file=...`，实现交互渲染 + 跟随系统深色主题
- 修复 `ARCHITECTURE.md` 4 处 Mermaid 语法错误（`{}`/`[[...]]`/裸 `"` 在 `[]` 标签内）
- `md-viewer.html` 吞错 `catch` 改为红色错误横幅 + `console.error`，以后解析失败不再隐入无声

### 测试
- `internal/server/e2e_test.go`：`NewQuickGateway` 调用更新到 10 参数签名
# agent-proxy 用户手册

## 目录

1. [概述](#概述)
2. [安装](#安装)
3. [快速模式使用](#快速模式使用)
4. [复杂模式使用](#复杂模式使用)
5. [CLI 命令参考](#cli-命令参考)
6. [协议兼容性详解](#协议兼容性详解)
7. [Web UI 使用](#web-ui-使用)
8. [高级配置](#高级配置)
9. [故障排查](#故障排查)
10. [扩展开发](#扩展开发)

---

## 概述

agent-proxy 是一个 AI 消息协议网关，提供两种启动模式：

```
┌── 快速模式（--mode simple）── 从 SQLite 选一条记录，立即启动 ──┐
│  命令: agent-proxy run --mode simple --db 1                     │
│  适用：单一端点、快速试用、脚本调用                               │
│  特点：零配置、开箱即用、自动协议识别                             │
└────────────────────────────────────────────────────────────────┘

┌── 复杂模式（--mode complex）── 多 Provider、路由、Web UI ──┐
│  命令: agent-proxy run --mode complex                          │
│        agent-proxy run --mode complex --conf config.json       │
│  适用：生产环境、多厂商调度、需要监控                           │
│  特点：路由前缀匹配、限流、实时指标、美观面板                   │
└────────────────────────────────────────────────────────────────┘
```

### 核心设计

- **Central Schema 中枢模型**：定义与所有外部协议无关的统一消息结构
- **双向翻译器**：每个协议实现 请求→中枢、中枢→请求、响应→中枢、中枢→响应、流式→中枢
- **Provider 统一接口**：`Call()` / `CallStream()` 屏蔽下游差异

---

## 安装

### 二进制

直接运行 `agent-proxy` 或 `agent-proxy.exe`，无需额外依赖：

```powershell
# 确认运行正常
.\agent-proxy.exe --help
```

### 从源码编译

```powershell
go mod download
go build -o agent-proxy ./cmd/server
```

依赖：

| 包 | 用途 |
|----|------|
| `github.com/go-chi/chi/v5` | HTTP 路由 |
| `modernc.org/sqlite` | 纯 Go SQLite（无需系统库） |

---

## 快速模式使用

### 30 秒上手

```powershell
# ① 添加一个代理（自动嗅探模型列表）
agent-proxy db add --url https://token.sensenova.cn/v1 \
                   --key sk-b9ffyFsWinZg7QSOkMfF6P4gEXQ2mFKf \
                   --name sensenova

# ② 快速模式启动
agent-proxy run --mode simple --host 0.0.0.0 --port 8080 --db 1

# ③ 使用（标准 OpenAI Chat Completions）
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"sensenova-6.7-flash-lite","messages":[{"role":"user","content":"hello"}]}'
```

### 数据库存储

数据库文件位于 `~/.agent-proxy/proxies.db`（SQLite 单文件）：

```sql
CREATE TABLE proxies (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  name            TEXT NOT NULL DEFAULT '',
  url             TEXT NOT NULL,
  key             TEXT NOT NULL,
  provider_type   TEXT NOT NULL DEFAULT 'openai',
  detected_format TEXT,
  openai_cap      INTEGER NOT NULL DEFAULT 0,
  anthropic_cap   INTEGER NOT NULL DEFAULT 0,
  model_count     INTEGER NOT NULL DEFAULT 0,
  models_json     TEXT,
  weight          INTEGER NOT NULL DEFAULT 100,
  created_at      TEXT NOT NULL
);
```

> **安全提示**：Key 以明文存储在 SQLite 中。不要将 `.db` 文件上传到公开仓库。

### 多 Provider 快速模式

快速模式**只支持一条记录**。如需多 Provider 调度，使用复杂模式。

---

## 复杂模式使用

### 默认配置启动

```powershell
# 设置主代理 Key
$env:AGENT_PROXY_API_KEY = "sk-your-key-here"

# 复杂模式启动
agent-proxy run --mode complex --host 0.0.0.0 --port 8080
```

控制台输出：

```
🚀 Agent-Proxy (复杂模式) running on http://0.0.0.0:8080
📊 Web UI: http://localhost:8080/ui
📝 Chat Completions: POST http://localhost:8080/v1/chat/completions
```

### 配置文件启动

```powershell
agent-proxy run --mode complex --host 0.0.0.0 --port 8080 --conf config.json
```

配置文件示例见 `config.example.json`。

### 自定义监听地址

```powershell
agent-proxy run --mode complex --host 127.0.0.1 --port 9090
```

### 多 Provider 配置

在 `cmd/server/main.go` 中修改 `startComplexMode()` 函数：

```go
cfg.Providers["my-provider"] = &config.ProviderConfig{
    BaseURL:      "https://api.example.com",
    APIToken:     os.Getenv("MY_API_KEY"),
    ProviderType: "openai",   // openai / anthropic / gemini
    Models:       []string{"model-1", "model-2"},
    Weight:       100,
    TimeoutSec:   60,
    RateLimit:    100,
}
```

### 模型路由

```go
cfg.ModelRouter.ModelToProvider = map[string]string{
    "sensenova-": "sensenova",   // 前缀匹配
    "claude-":    "anthropic",
}
cfg.ModelRouter.DefaultProvider = "sensenova"  // 兜底
```

匹配逻辑：遍历路由表，前缀匹配 → 精确匹配 → 默认 Provider。

---

## CLI 命令参考

### 启动

| 命令 | 说明 |
|------|------|
| `agent-proxy run --mode complex --host 0.0.0.0 --port 8080` | 复杂模式（默认配置） |
| `agent-proxy run --mode complex --conf config.json` | 复杂模式 + 配置文件 |
| `agent-proxy run --mode simple --host 0.0.0.0 --port 8080 --db 1` | 快速模式 |
| `agent-proxy --help` | 显示帮助 |

### DB 管理

| 命令 | 示例 | 说明 |
|------|------|------|
| `db list` | `agent-proxy db list` | 列出所有代理 |
| `db show` | `agent-proxy db show 1` | 查看 ID=1 详情 |
| `db add` | `agent-proxy db add --url <url> --key <key>` | 添加代理 |
| `db add` | `agent-proxy db add --url <url> --key <key> --name my --type anthropic` | 添加（指定类型） |
| `db rm` | `agent-proxy db rm 1` | 删除记录 |

### `add` 参数详解

| 参数 | 默认值 | 必填 | 说明 |
|------|--------|------|------|
| `--url` | — | ✅ | API 地址，如 `https://api.example.com/v1` |
| `--key` | — | ✅ | API Key |
| `--name` | URL 值 | 否 | 友好名称 |
| `--type` | `openai` | 否 | `openai` / `anthropic` / `gemini` |

添加时自动执行：

1. 拼接 `/v1/models` 端点，发送 GET 请求
2. 解析返回的模型列表
3. 写入 SQLite（含模型 JSON、时间戳）

### `show` 输出示例

```
🔍 代理配置详情:
  ID:           1
  Name:         sensenova
  URL:          https://token.sensenova.cn/v1
  Key:          sk-b9****2mFKf
  Provider:     openai
  检测格式:     openai-compatible
  OpenAI:       true
  Anthropic:    false
  权重:         100
  时间:         2026-08-03 17:30:00
  模型列表 (4):
      1. sensenova-6.7-flash-lite
      2. deepseek-v4-flash
      3. glm-5.2
      4. sensenova-u1-fast
```

### 数据库文件位置

| 平台 | 路径 |
|------|------|
| Windows | `%USERPROFILE%\.agent-proxy\proxies.db` |
| macOS / Linux | `~/.agent-proxy/proxies.db` |
| 自定义 | `--dbpath /path/to/custom.db` |

---

## 协议兼容性详解

### Central Schema

所有外部协议先转为 `schema.InternalRequest`，再转为下游格式：

```
ChatCompletionRequest  ─→  InternalRequest  ─→  AnthropicRequest
                              (中枢)
ChatCompletionRequest  ─→  InternalRequest  ─→  GeminiRequest
```

### InternalMessage 结构

```go
type InternalMessage struct {
    Role    MessageRole         `json:"role"`     // "system" | "user" | "assistant" | "tool"
    Content json.RawMessage     `json:"content"`  // 保留原始结构，避免信息丢失
    ToolCalls []InternalToolCall `json:"tool_calls,omitempty"`
    ToolCallID string           `json:"tool_call_id,omitempty"`
    Name    string              `json:"name,omitempty"`
}
```

### 8 大差异点处理

| # | 差异点 | CC 入口 | Anthropic 输出 | Gemini 输出 |
|---|--------|---------|---------------|-------------|
| 1 | System prompt | 提取到 `internalReq.System` | 顶层 `system` 字段 | `systemInstruction` |
| 2 | Tool 定义 | `tools[].function.parameters` | `tools[].input_schema` | `tools[].functionDeclarations[].parameters` |
| 3 | Tool call | `tool_calls[]` 独立 | `content[]` 混入 `type: "tool_use"` | `parts[]` 混入 `functionCall` |
| 4 | Tool args | `arguments` 是 JSON 字符串 | `input` 是 JSON 对象 | `args` 是 JSON 对象 |
| 5 | Tool result | `role: "tool"` | `role: "user"` + `tool_use_id` | `role: "user"` + `functionResponse` |
| 6 | Usage | `prompt_tokens` | `input_tokens` → 映射回 | `prompt_token_count` → 映射回 |
| 7 | Stop reason | `stop` / `length` | `end_turn` → `stop`；`max_tokens` → `length` | 同 |
| 8 | SSE 流式 | 无 `event:` 行 | `type` 字段区分 | 标准 SSE |

### 流式请求

网关支持透传 SSE 流，下游事件原样转发（不翻译每一行）。元数据事件（`_type: "headers"`）会被解析并透传响应头。

---

## Web UI 使用

### 访问

```
http://localhost:8080/ui
```

### 功能

| 区域 | 内容 | 更新方式 |
|------|------|---------|
| 顶部指标卡 | QPS / P99 延迟 / 错误率 / 活跃连接 | 2 秒轮询 |
| Provider 列表 | 名称 / 状态圆点 / 请求数 / 平均延迟 | 2 秒轮询 |
| QPS 图表 | 60 秒趋势 | SSE 推送 |
| 延迟图表 | 60 秒趋势 | SSE 推送 |
| 请求日志 | 时间 / 模型 / Provider / 状态 / 耗时 | SSE 推送 |

### Provider 状态

| 颜色 | 含义 |
|------|------|
| 🟢 绿 | 健康（有成功请求） |
| 🟡 黄 | 降级（部分失败） |
| 🔴 红 | 宕机（全部失败） |
| ⚪ 白 | 空闲（无请求） |

### API 端点

| 端点 | 方法 | 格式 | 说明 |
|------|------|------|------|
| `/ui/api/summary` | GET | JSON | 全量指标快照 |
| `/ui/api/logs` | GET | SSE | 实时请求日志 |
| `/ui/api/metrics` | GET | SSE | 实时指标流 |
| `/ui/api/providers` | GET | JSON | Provider 状态列表 |

---

## 高级配置

### 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `AGENT_PROXY_API_KEY` | — | 主代理 API Key |
| `ANTHROPIC_API_KEY` | — | Anthropic API Key |
| `AGENT_PROXY_DB_PATH` | `~/.agent-proxy/proxies.db` | 数据库路径覆盖 |

### 限流配置

```go
type RateLimitConfig struct {
    RequestsPerSecond int `yaml:"requests_per_second"`
    Burst             int `yaml:"burst"`
}
```

使用令牌桶算法，默认 100 QPS / 200 burst。

### Provider 超时

```go
type ProviderConfig struct {
    TimeoutSec int `yaml:"timeout_sec"`    // 调用超时
    RateLimit  int `yaml:"rate_limit"`     // 本地限流
}
```

### 日志

所有请求日志输出到 stdout：

```
[17:30:16] sensenova-6.7-flash-lite → sensenova 5234ms
```

---

## 故障排查

### 问题 1：启动后无法连接 Provider

```
原因：API Key 未设置或失效
解决：检查环境变量 / DB 中记录的 Key
```

```powershell
# 快速检查
curl -H "Authorization: Bearer $env:AGENT_PROXY_API_KEY" https://api.example.com/v1/models
```

### 问题 2：URL 404（/v1/v1/...）

```
原因：URL 末尾带 /v1，BuildURL 又追加了一次
解决：DB 中 URL 去掉末尾 /v1，或使用 normalizeBaseURL
```

### 问题 3：快速模式报错 "未找到 ID"

```
原因：DB 为空或 ID 不存在
解决：
  .\agent-proxy.exe list   # 查看现有记录
  .\agent-proxy.exe add    # 添加新记录
```

### 问题 4：端口被占用

```
原因：另一个 agent-proxy 实例仍在运行
解决：
  netstat -ano | Select-String ":8080"
  taskkill /F /PID <pid>
  # 或启动时指定其他端口
  .\agent-proxy.exe --port 9090
```

### 问题 5：流式请求卡住

```
原因：下游无 SSE 响应或连接超时
解决：
  1. 降低 TimeoutSec
  2. 检查网络连通性
  3. 确认下游支持 stream=true
```

---

## 扩展开发

### 新增协议

1. **定义类型**（`internal/protocol/mymodule/types.go`）：

```go
package mymodule

type MyRequest struct {
    Messages []Message `json:"messages"`
    Model    string    `json:"model"`
}
```

2. **实现翻译器**（`internal/protocol/mymodule/translator.go`）：

```go
type MyTranslator struct{}

func (t *MyTranslator) TranslateRequest(raw json.RawMessage) (*schema.InternalRequest, error) {
    // ... 解析 → 中枢
}

func (t *MyTranslator) TranslateToProvider(req *schema.InternalRequest) (json.RawMessage, error) {
    // ... 中枢 → 下游格式
}

func (t *MyTranslator) TranslateFromProvider(raw json.RawMessage) (*schema.InternalResponse, error) {
    // ... 下游格式 → 中枢
}

func (t *MyTranslator) TranslateResponse(resp *schema.InternalResponse) (json.RawMessage, error) {
    // ... 中枢 → 响应
}

func (t *MyTranslator) TranslateStream(ctx context.Context, events <-chan schema.InternalStreamEvent, fn func([]byte, bool)) {
    // ... 中枢事件 → SSE
}

func (t *MyTranslator) TranslateError(err *schema.StreamError) json.RawMessage {
    // ... 中枢错误 → 错误 JSON
}
```

3. **注册 Provider 客户端**（`internal/provider/mymodule.go`）：

```go
type MyClient struct {
    baseURL  string
    timeout  int
}

func NewMyClient(name, baseURL string, timeout int) *MyClient { ... }

func (c *MyClient) Call(ctx context.Context, body json.RawMessage, info *schema.ProviderInfo) (json.RawMessage, map[string][]string, error) {
    // ... HTTP 调用
}

func (c *MyClient) CallStream(ctx context.Context, body json.RawMessage, info *schema.ProviderInfo) (<-chan json.RawMessage, map[string][]string, error) {
    // ... SSE 流
}
```

4. **在 `gateway.go` 中注册路由**：

```go
case "mymodule":
    providerRegistry.Register(provider.NewMyClient(name, pc.BaseURL, pc.TimeoutSec))
```

### 架构原则

```
┌──────────────────────────────────────────────┐
│  所有翻译路径都经过 Central Schema（internal）   │
│                                              │
│  禁止：协议 A 直接翻译到 协议 B                 │
│  必须：协议 A → Schema → 协议 B                │
│                                              │
│  这保证 N 个协议只需 N 个翻译器                 │
│  而非 N×N 个                                   │
└──────────────────────────────────────────────┘
```

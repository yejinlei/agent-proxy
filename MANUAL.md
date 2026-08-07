# agent-proxy 用户手册

## 目录

1. [概述](#概述)
2. [安装](#安装)
3. [快速模式使用](#快速模式使用)
4. [复杂模式使用](#复杂模式使用)
5. [CLI 命令参考](#cli-命令参考)
6. [客户端认证](#客户端认证)
7. [协议兼容性详解](#协议兼容性详解)
8. [Web UI 使用](#web-ui-使用)
9. [高级配置](#高级配置)
10. [故障排查](#故障排查)
11. [扩展开发](#扩展开发)

---

## 概述

agent-proxy 是一个 **4×4 全协议互转** 的 AI 消息协议网关，提供两种启动模式：

```
┌── 快速模式（--mode simple）── 从 SQLite 选一条记录，立即启动 ──┐
│  命令: agent-proxy run --mode simple --db 1                     │
│  适用：单一端点、快速试用、脚本调用                               │
│  特点：零配置、开箱即用、4×4 协议自动识别与互转                   │
└────────────────────────────────────────────────────────────────┘

┌── 复杂模式（--mode complex）── 多 Provider、路由、Web UI ──┐
│  命令: agent-proxy run --mode complex                          │
│        agent-proxy run --mode complex --conf config.json       │
│  适用：生产环境、多厂商调度、需要监控                           │
│  特点：路由前缀匹配、限流、实时指标、4×4 全协议互转、美观面板   │
└────────────────────────────────────────────────────────────────┘
```

### 4×4 全协议互转

OpenAI Compatible、Anthropic Messages、Google Gemini、OpenAI Responses **任意入站可转任意出站**：

| 入站 ↓ \ 出站 → | OpenAI Compatible | Anthropic Messages | Google Gemini | OpenAI Responses |
|----------------|------------------|-------------------|---------------|-----------------|
| OpenAI Compatible | ✅ 透传 | ✅ 双向翻译 | ✅ 双向翻译 | ✅ 双向翻译 |
| Anthropic Messages | ✅ 双向翻译 | ✅ 透传 | ✅ 双向翻译 | ✅ 双向翻译 |
| Google Gemini | ✅ 双向翻译 | ✅ 双向翻译 | ✅ 透传 | ✅ 双向翻译 |
| OpenAI Responses | ✅ 双向翻译 | ✅ 双向翻译 | ✅ 双向翻译 | ✅ 透传 |

### 核心设计

- **Central Schema 中枢模型**：定义与所有外部协议无关的统一消息结构
- **CombinedTranslator 接口**：每个协议实现 `TranslateRequest` / `TranslateResponse` / `TranslateStream` 双向翻译
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
go build -o agent-proxy .
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
# ① 嗅探并添加代理（自动探测所有支持的协议）
agent-proxy db add --url https://token.sensenova.cn/v1 \
                   --key sk-b9ffyFsWinZg7QSOkMfF6P4gEXQ2mFKf \
                   --name sensenova
# 等价别名
agent-proxy detect --url https://token.sensenova.cn/v1 \
                   --key sk-b9ffyFsWinZg7QSOkMfF6P4gEXQ2mFKf \
                   --name sensenova

# ② 快速模式启动（--nokey 本地开发无需客户端密钥）
agent-proxy run --db 1 --nokey

# ③ 使用任意入站协议调用（4 种协议全支持，自动翻译到下游）
# 3a. OpenAI Compatible 入站
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"sensenova-6.7-flash-lite","messages":[{"role":"user","content":"hello"}]}'

# 3b. Anthropic Messages 入站
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"sensenova-6.7-flash-lite","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}'

# 3c. Google Gemini 入站
curl -X POST "http://localhost:8080/v1/models/sensenova-6.7-flash-lite:generateContent" \
  -H "Content-Type: application/json" \
  -d '{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}'

# 3d. OpenAI Responses 入站
curl -X POST http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -d '{"model":"sensenova-6.7-flash-lite","input":[{"type":"message","role":"user","content":"hello"}]}'
```

> **注意**：以上 curl 示例省略了 `Authorization` 头，因为启动时使用了 `--nokey`。默认情况下快速模式会要求客户端认证，详见下方[客户端认证](#客户端认证)章节。

### 客户端认证

快速模式默认要求客户端通过 `Authorization: Bearer <key>` 头认证后才能连接。三种使用方式：

#### 方式一：随机生成密钥（默认）

不传 `--key` 也不传 `--nokey`，启动时自动随机生成一个 48 位 hex 密钥并打印到控制台：

```powershell
agent-proxy run --db 1
```

```
🚀 Agent-Proxy (快速模式) running on http://127.0.0.1:8080
🔑 Proxy Key:      sk-a1b2c3d4e5f6...
🔐 客户端需使用 Authorization: Bearer sk-a1b2c3d4e5f6... 连接
```

```powershell
curl -X POST http://localhost:8080/v1/chat/completions ^
  -H "Authorization: Bearer sk-a1b2c3d4e5f6..." ^
  -H "Content-Type: application/json" ^
  -d '{"model":"sensenova-6.7-flash-lite","messages":[{"role":"user","content":"hi"}]}'
```

> 密钥每次重启都会变化，适合本地临时使用。如需固定密钥请用方式二。

#### 方式二：指定固定密钥

```powershell
agent-proxy run --db 1 --key sk-my-fixed-key
```

客户端连接时使用同一个 `sk-my-fixed-key`。

#### 方式三：无需密钥（本地开发用）

```powershell
agent-proxy run --db 1 --nokey
```

任何客户端均可直接连接，无需 `Authorization` 头。**不要在生产环境使用此模式。**

```powershell
curl -X POST http://localhost:8080/v1/chat/completions ^
  -H "Content-Type: application/json" ^
  -d '{"model":"sensenova-6.7-flash-lite","messages":[{"role":"user","content":"hi"}]}'
```

#### 认证行为说明

| 标志 | 密钥来源 | 客户端是否需要 `Authorization` 头 | 适用场景 |
|------|---------|----------------------------------|---------|
| （不传） | 随机生成 | ✅ 需要 | 本地临时使用、快速试用 |
| `--key <k>` | 指定值 | ✅ 需要 | 固定环境、自动化脚本 |
| `--nokey` | 无 | ❌ 不需要 | 本地开发、内网直连 |

- 缺密钥或密钥错误时返回 `401 Unauthorized`，响应体为标准错误格式（`error.type`, `error.message`）
- `/health` 端点始终**不受认证影响**，可用于健康检查或负载均衡探测
- 未认证的请求会被拒绝，不会到达下游 Provider

### 数据库存储

数据库文件位于 `~/.agent-proxy/proxies.db`（SQLite 单文件），含多协议嗅探列：

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
  capabilities_json TEXT,   -- 嗅探到的所有协议 ["openai","anthropic","gemini","responses"]
  models_map_json   TEXT,   -- 每协议模型 {"openai":["gpt-4"],"anthropic":["claude-3"]}
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
| `agent-proxy run --db <id>` | 快速模式（默认，随机密钥） |
| `agent-proxy run --db <id> --key <k>` | 快速模式（指定密钥） |
| `agent-proxy run --db <id> --nokey` | 快速模式（无需密钥） |
| `agent-proxy run --mode complex --host 0.0.0.0 --port 8080` | 复杂模式（默认配置） |
| `agent-proxy run --mode complex --conf config.json` | 复杂模式 + 配置文件 |
| `agent-proxy run --mode simple --host 0.0.0.0 --port 8080 --db 1` | 快速模式 |
| `agent-proxy --help` | 显示帮助 |

### DB 管理

| 命令 | 示例 | 说明 |
|------|------|------|
| `db list` | `agent-proxy db list` | 列出所有代理 |
| `db show` | `agent-proxy db show 1` | 查看 ID=1 详情 |
| `db add` / `db detect` | `agent-proxy db add --url <url> --key <key> [--name <n>]` | 嗅探并添加（自动探测所有协议，确认后写入） |
| `db rm` | `agent-proxy db rm 1` | 删除记录 |

### `add` / `detect` 参数详解

| 参数 | 默认值 | 必填 | 说明 |
|------|--------|------|------|
| `--url` | — | ✅ | API 地址，如 `https://api.example.com/v1` |
| `--key` | — | ✅ | API Key |
| `--name` | URL 值 | 否 | 友好名称 |

`--type` 参数已废弃（协议由嗅探自动确定）。

添加时自动执行：

1. **嗅探 OpenAI**：`GET {url}/v1/models` + Bearer → 200/401 视为支持
2. **自动标记 Responses**：OpenAI 成功则同时标记
3. **嗅探 Anthropic**：`POST {url}/v1/messages` + x-api-key
4. **嗅探 Gemini**：`POST {url}/v1/models/gemini-pro:generateContent`
5. **汇总**：至少 1 个模型 → 提示确认 → 写入 SQLite
6. **失败**：0 模型 → 直接报告失败，不提示

### 协议感知路由

启动快速模式后，每个入站请求按以下逻辑路由：

```
入站协议 → normalizeIngress → selectProtocol(capabilities)
  - 命中 capabilities → 透传（零开销转发）
  - 未命中          → 回退到 OpenAI 协议转换
```

示例：
- 上游支持 openai + anthropic，下游按 Anthropic `/v1/messages` 接入 → **透传**
- 上游仅支持 openai，下游按 Gemini 接入 → 经 OpenAI 翻译后转发
- 上游支持 openai，下游按 Responses 接入 → 透传（Responses 与 OpenAI 共享）

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

### 透传优化（Passthrough）

当 **入站协议 == 下游 Provider 类型** 时，网关**跳过翻译链路，直接透传原始请求体**给下游，避免不必要的格式转换：

```
入站协议 == Provider 类型  ─→  直接转发原始 body（零损耗、零开销）
                              模型名 → 注入 URL 路径 / 请求头
入站协议 ≠ Provider 类型   ─→  走完整翻译链路（如上）
```

示例：Anthropic Messages 入站 → Anthropic Provider 出站，请求体原样发送，不做任何转换。

透传路径要点：
- 请求体 (`json.RawMessage`) 不经翻译器，直接传给 `ProviderClient.Call` / `CallStream`
- 模型名通过防御拷贝的 `ProviderInfo.Name` 注入下游 URL（如 Gemini `/v1/models/{model}:generateContent`）
- 响应体原样回传，仅过滤内部元数据事件（`_type="headers"`）
- 请求体在入口处已预读取为内存，透传时重包为可读流，避免 body 耗尽

完整翻译链路（协议不匹配时）仍为：

```
入站协议 TranslateRequest → InternalRequest → 路由 Provider
→ TranslateToProvider → Provider 调用
→ TranslateFromProvider → InternalResponse
→ 入站协议 TranslateResponse → 出站响应
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

### 流式请求（SSE 翻译链路）

流式请求同样经过完整的翻译链路，不是简单透传：

```
下游 Provider SSE 行 → TranslateStreamEvent → InternalStreamEvent
→ 入站协议 TranslateStream → 上游 SSE 输出（匹配入站协议格式）
```

各协议 SSE 格式差异在网关内自动适配：
- **CC 格式**：纯 `data: {...}\n\n`，无 event 行，以 `data: [DONE]` 结尾
- **Anthropic 格式**：每行带 `type` 字段（`message_start` / `content_block_delta` / `message_delta`）
- **Gemini 格式**：每行是完整 `StreamChunk` 对象，带 `candidates` 数组
- **Responses 格式**：带 named events（`event: response.output_delta` 等）

元数据事件（`_type: "headers"`）会被解析并透传响应头。

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

func (t *MyTranslator) TranslateRequest(ctx context.Context, raw json.RawMessage) (*schema.InternalRequest, error) {
    // ... 解析 → 中枢（路径元数据如模型名通过 ctx 传入）
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

# agent-proxy — AI 消息协议网关

## 一句话

agent-proxy 是 AI 协议翻译的中间件：将上游 LLM 端点的 OpenAI / Anthropic / Gemini / Responses API **统一转化为 Chat Completions 格式**，让你用一个接口打通所有厂商。

两种模式覆盖所有场景：

- **快速模式**：`--db N` 一条命令启动，从 SQLite 选一个记录，直接转发
- **复杂模式**：多 Provider 路由、前缀匹配、Web UI 实时监控

## 架构

```
┌──────────────────────────────────────────────────────────┐
│                   agent-proxy                             │
│  ┌──────────────┐  ┌──────────┐  ┌──────────────────┐   │
│  │ Chat         │  │ Internal │  │ Protocol         │   │
│  │ Completion   │←→│ Schema   │←→│ Translators      │   │
│  │ (入口协议)   │  │ (中枢)   │  │ Anthropic        │   │
│  │              │  │          │  │ Gemini           │   │
│  │              │  │          │  │ OpenAI Responses │   │
│  └──────┬───────┘  └────┬─────┘  └────────┬─────────┘   │
│         │               │                  │             │
│  ┌──────▼───────────────▼──────────────────▼─────────┐  │
│  │          Provider Clients                         │  │
│  │  OpenAIClient │ AnthropicClient │ GeminiClient   │  │
│  └──────────────────────────────────────────────────┘  │
│         │                                               │
│  ┌──────▼──────────────────────────────┐               │
│  │          Model Router               │               │
│  │  prefix match → provider            │               │
│  └─────────────────────────────────────┘               │
│                                                        │
│  ┌──────────────────────────────────────┐             │
│  │  Web UI (embed.FS)                   │             │
│  │  /ui  → 实时监控面板                 │             │
│  └──────────────────────────────────────┘             │
└──────────────────────────────────────────────────────────┘
```

- **Central Schema**：与所有外部协议无关的统一消息模型，所有翻译通过此模型中转
- **Protocol Translators**：每个协议实现请求翻译 / 响应翻译 / 流式翻译
- **Provider Clients**：统一的下游调用接口
- **Model Router**：模型前缀匹配到 Provider
- **Web UI**：嵌入静态资源，实时指标推送

## 快速开始

```powershell
# === 复杂模式（默认） ===
# 设置环境变量
$env:AGENT_PROXY_API_KEY = "sk-your-key-here"

# 启动（默认 0.0.0.0:8080）
.\agent-proxy.exe

# 自定义端口
.\agent-proxy.exe --host 0.0.0.0 --port 9090


# === 快速模式：先添加代理，再启动 ===

# 1. 添加代理（自动嗅探模型列表）
.\agent-proxy.exe add --url https://token.sensenova.cn/v1 \
                      --key sk-xxx \
                      --name sensenova \
                      --type openai

# 2. 查看已保存的代理
.\agent-proxy.exe list

# 3. 快速启动（使用 DB 第 1 条记录）
.\agent-proxy.exe --db 1

# 4. 测试
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"sensenova-6.7-flash-lite\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}"
```

## 命令总览

```
agent-proxy [command] [options]

启动模式：
  agent-proxy                           # 复杂模式启动（默认配置）
  agent-proxy --db <id>                 # 快速模式：从 DB 选一条记录
  agent-proxy --host <h> --port <p>     # 指定监听地址
  agent-proxy --dbpath <path>           # 指定数据库路径

CLI 命令（SQLite DB 管理）：
  agent-proxy list                      # 列出所有代理配置
  agent-proxy show <id>                 # 显示指定代理详情
  agent-proxy add --url <url> --key <key> [--name <n>] [--type <t>]
                                        # 添加代理配置（自动嗅探模型列表）
  agent-proxy rm <id>                   # 删除代理配置

  --help  /  -h                         # 显示帮助
```

详细用法见 [MANUAL.md](MANUAL.md)。

## 快速模式 vs 复杂模式

| 维度 | 快速模式（`--db N`） | 复杂模式（默认） |
|------|---------------------|-----------------|
| 启动方式 | `.\agent-proxy.exe --db 1` | `.\agent-proxy.exe` |
| Provider 来源 | SQLite 数据库一条记录 | 内置配置（可改） |
| 多 Provider | 不支持 | 支持（路由前缀匹配） |
| 协议翻译 | 支持（自动按 Provider 类型） | 支持（完整 4 协议） |
| Web UI | 无 | 有（`/ui`） |
| 限流 / 监控 | 无 | 有 |
| 适用场景 | 快速试用、单一端点 | 生产、多厂商调度 |

## 支持的协议

| 上游协议 | 网关端点 | 自动翻译 |
|---------|---------|---------|
| OpenAI Compatible | `POST /v1/chat/completions` | ✅ 透传 |
| Anthropic Messages | `POST /v1/messages` | ✅ 请求/响应/流式 |
| Google Gemini | `POST /v1/models/{model}:generateContent` | ✅ 请求/响应/流式 |
| OpenAI Responses | `POST /v1/responses` | ✅ 请求/响应/流式 |

## 协议兼容性差异点

agent-proxy 处理以下 8 大协议差异：

| # | 差异点 | 说明 |
|---|--------|------|
| 1 | System prompt 位置 | CC 在 messages 中；Anthropic 顶层 system；Gemini systemInstruction |
| 2 | Tool 定义字段 | parameters vs input_schema vs functionDeclarations |
| 3 | Tool call 位置 | CC 独立 tool_calls；Anthropic/Gemini 混在 content blocks |
| 4 | Tool call arguments | CC 是 JSON 字符串；其他是 JSON 对象 |
| 5 | Tool result 角色 | Anthropic 归 user；Gemini 归 user + functionResponse |
| 6 | Usage 字段名 | prompt_tokens vs input_tokens vs prompt_token_count |
| 7 | Stop reason | end_turn → stop；max_tokens → length |
| 8 | SSE 事件格式 | CC 无 event 行；Anthropic 用 type 字段 |

## CLI 命令速查

### 添加代理

```powershell
# 自动嗅探模型并保存到 DB（推荐）
.\agent-proxy.exe add --url https://api.example.com/v1 \
                      --key sk-xxx \
                      --name my-provider \
                      --type openai
```

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--url` | （必填） | Provider API 地址 |
| `--key` | （必填） | API Key |
| `--name` | URL 值 | 别名 |
| `--type` | `openai` | `openai` / `anthropic` / `gemini` |

### 查询 / 管理

```powershell
# 列出所有
.\agent-proxy.exe list

# 查看详情
.\agent-proxy.exe show 1

# 删除（需确认）
.\agent-proxy.exe rm 1
```

## 配置

复杂模式下通过环境变量配置 Provider：

```powershell
# 主代理（Sensenova / OpenAI 兼容）
$env:AGENT_PROXY_API_KEY = "sk-your-key"

# Anthropic（可选）
$env:ANTHROPIC_API_KEY = "sk-ant-xxx"
```

默认配置（`cmd/server/main.go` 中可自定义）：

| Provider | URL | 类型 | 默认模型 |
|----------|-----|------|---------|
| sensenova | `https://token.sensenova.cn` | openai | sensenova-6.7-flash-lite 等 |
| anthropic | `https://api.anthropic.com` | anthropic | claude-3-5-sonnet 等 |

## 模型路由

复杂模式通过模型前缀匹配 Provider：

| 前缀 | 路由到 |
|------|--------|
| `sensenova-` | sensenova |
| `deepseek-` | sensenova |
| `glm-` | sensenova |
| `claude-` | anthropic |
| `gemini-` | gemini（需配置） |

## Web UI

复杂模式启动后访问 `http://localhost:8080/ui`，深色主题面板：

- **实时指标卡片**：QPS / P99 延迟 / 错误率 / 活跃连接
- **Provider 状态列表**：健康状态圆点（green/degraded/down/idle）
- **图表**：60 秒 QPS + 延迟趋势（uPlot）
- **请求日志**：SSE 推送，含模型/状态/耗时
- **数据接口**：`/ui/api/summary` `/ui/api/logs` `/ui/api/metrics` `/ui/api/providers`

## 性能与部署

- 单二进制部署（`embed.FS` 嵌入静态资源）
- 零外部依赖运行时（除 SQLite 为纯 Go 实现）
- 连接池 / 限流器（令牌桶）
- 优雅关闭（SIGINT / SIGTERM）

## 项目结构

```
agent-proxy/
├── main.go (cmd/server/)           # 入口 + 双模式启动
├── cli.go (cmd/)                   # CLI 命令实现
├── agent-proxy                     # Windows 二进制
├── go.mod / go.sum
├── .env.example                    # 环境变量示例
└── internal/
    ├── config/config.go            # 配置结构
    ├── db/db.go                    # SQLite 持久化
    ├── middleware/middleware.go    # 中间件（Logger/CORS/Auth）
    ├── monitor/store.go            # 监控指标存储
    ├── protocol/
    │   ├── schema/internal.go      # 中枢消息模型
    │   ├── chatcompletion/         # CC 协议定义 + 翻译器
    │   ├── anthropic/              # Anthropic Messages
    │   ├── gemini/                 # Google Gemini
    │   └── responses/              # OpenAI Responses
    ├── provider/
    │   ├── provider.go             # Provider 接口
    │   └── openai.go               # 客户端实现
    ├── router/router.go            # 模型路由器
    ├── server/
    │   ├── gateway.go              # 复杂模式网关
    │   ├── quick.go                # 快速模式网关
    │   └── ratelimiter.go          # 令牌桶限流
    ├── translator/interfaces.go    # 翻译器接口
    └── web/
        ├── server.go               # Web UI + API
        └── static/                 # 嵌入静态资源
            ├── index.html
            ├── dashboard.css
            └── dashboard.js
```

## 扩展新协议

1. 在 `internal/protocol/` 下新建目录，定义 `types.go`
2. 实现 `translator.go`，注册到 `CombinedTranslator` 接口
3. 在 `internal/provider/` 下添加对应的 Client
4. 在 `internal/translator/interfaces.go` 注册

## License

MIT

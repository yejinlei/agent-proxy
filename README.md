# agent-proxy — AI 协议网关

> **4×4 全协议互转** · 单二进制部署 · 零外部依赖

agent-proxy 是一个 AI 消息协议中间件：任意入站协议（OpenAI / Anthropic / Gemini / Responses）可转换为任意出站协议，彻底打通不同厂商的 API 格式差异。

```
┌──────────────────────────────────────────────────────────────────┐
│                                                                   │
│        入站 (任何协议)     agent-proxy     出站 (任何协议)        │
│                                                                   │
│  ┌──────────┐               ┌────────────┐               ┌────────┐│
│  │  OpenAI  │──────────────→│  Central   │──────────────→│ OpenAI ││
│  │  Anth.   │──────────────→│  Schema    │──────────────→│ Anth.  ││
│  │  Gemini  │──────────────→│  (中枢)    │──────────────→│ Gemini ││
│  │ Resps.   │──────────────→│            │──────────────→│ Resps. ││
│  └──────────┘               └────────────┘               └────────┘│
│                                                                   │
└──────────────────────────────────────────────────────────────────┘
```

---

## ✨ 特性

| 特性 | 说明 |
|------|------|
| 🔄 **4×4 协议互转** | OpenAI / Anthropic / Gemini / Responses 任意入站 → 任意出站 |
| ⚡ **透传优化** | 入站协议与下游匹配时零开销直传，不做多余翻译 |
| 🔎 **智能嗅探** | 添加代理时自动探测所有支持的协议和模型 |
| 🚀 **快速模式** | `agent-proxy run --db 1` 一条命令启动，零配置 |
| 🏗️ **复杂模式** | 多 Provider 路由、限流、Web UI 实时监控 |
| 📦 **单二进制** | 纯 Go 编译，内嵌 SQLite，无外部依赖 |
| 🛡️ **客户端认证** | 快速模式内置 Bearer Token 认证 |
| 📊 **Web UI** | 深色主题面板：实时指标、请求日志、Provider 状态 |

---

## 🚀 5 分钟上手

### 1. 嗅探并添加代理

自动探测上游支持的所有协议：

```bash
agent-proxy db add \
  --url https://token.sensenova.cn/v1 \
  --key sk-xxx \
  --name sensenova
```

```
🔎 探测到 4 个协议，12 个模型：
  · openai (4 个模型)
  · responses (4 个模型)
  · anthropic (0 个模型)
  · gemini (4 个模型)
  是否要加到 DB？(yes/no): yes
✅ 已保存代理配置
```

### 2. 启动代理

```bash
agent-proxy run --db 1 --nokey
```

> `--nokey` 跳过客户端认证，适合本地开发。生产环境建议省略或指定 `--key`。

### 3. 任意协议调用

启动后网关同时暴露 4 种协议入口：

```bash
# ① OpenAI Compatible
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"sensenova-6.7-flash-lite","messages":[{"role":"user","content":"hello"}]}'

# ② Anthropic Messages
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: dummy" \
  -d '{"model":"sensenova-6.7-flash-lite","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}'

# ③ Google Gemini
curl -X POST "http://localhost:8080/v1/models/sensenova-6.7-flash-lite:generateContent" \
  -H "Content-Type: application/json" \
  -d '{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}'

# ④ OpenAI Responses
curl -X POST http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -d '{"model":"sensenova-6.7-flash-lite","input":[{"type":"message","role":"user","content":"hello"}]}'
```

---

## 📋 CLI 命令参考

```
agent-proxy <command> [options]
```

### 启动

| 命令 | 说明 |
|------|------|
| `run --db <id>` | 快速模式启动（从 DB 选一条记录） |
| `run --db <id> --key <k>` | 快速模式 + 指定客户端密钥 |
| `run --db <id> --nokey` | 快速模式，无需客户端密钥 |
| `run --mode complex` | 复杂模式（多 Provider + Web UI） |
| `run --mode complex --conf <f>` | 复杂模式 + 配置文件 |
| `run --mode complex --host <h> --port <p>` | 指定监听地址 |

### DB 管理

| 命令 | 说明 |
|------|------|
| `db add` | 新增代理（自动嗅探协议） |
| `db rm` | 删除代理 |
| `db query` | 查询代理（无 id 列出全部，有 id 显示详情） |
| `db find <关键词>` | 搜索代理（匹配名称 / URL / 协议 / 模型） |
| `db check` | 核对所有代理有效性 |
| `detect` | `db add` 的兼容别名 |

### DB 命令速查

```bash
# 新增代理
agent-proxy db add --url <url> --key <key> [--name <name>]

# 列出所有
agent-proxy db query

# 查看详情
agent-proxy db query 1

# 搜索代理（匹配名称 / URL / 协议 / 模型）
agent-proxy db find sensenova

# 核对有效性（重新嗅探每条记录，提示删除无效记录）
agent-proxy db check

# 删除记录
agent-proxy db rm 1
```

---

## ⚡ 快速模式 vs 复杂模式

| | 快速模式 (`--db`) | 复杂模式 (`--mode complex`) |
|---|---|---|
| **启动** | `agent-proxy run --db 1` | `agent-proxy run --mode complex` |
| **配置来源** | SQLite 一条记录 | 内置配置 / `--conf` 文件 |
| **多 Provider** | ❌ 单条 | ✅ 路由前缀匹配 |
| **协议互转** | ✅ 协议感知路由 | ✅ 4 协议完整支持 |
| **Web UI** | ❌ | ✅ `/ui` |
| **限流 / 监控** | ❌ | ✅ 令牌桶 + 实时指标 |
| **适用场景** | 快速试用、单一端点 | 生产、多厂商调度 |

---

## 🔐 客户端认证

快速模式内置认证，三种使用方式：

| 方式 | 命令 | 密钥来源 | 需 `Authorization` 头 |
|------|------|---------|---------------------|
| 随机密钥（默认） | `run --db 1` | 每次重启随机生成 | ✅ |
| 固定密钥 | `run --db 1 --key sk-xxx` | 用户指定 | ✅ |
| 无需密钥 | `run --db 1 --nokey` | 无 | ❌ |

```bash
agent-proxy run --db 1

# 输出：
# 🔑 Proxy Key: sk-a1b2c3d4e5f6...
# 🔐 客户端需使用 Authorization: Bearer sk-a1b2c3d4e5f6... 连接
```

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-a1b2c3d4e5f6..." \
  -H "Content-Type: application/json" \
  -d '{"model":"sensenova-6.7-flash-lite","messages":[{"role":"user","content":"hi"}]}'
```

> `/health` 端点始终不受认证影响。

---

## 🔮 协议感知路由

快速模式下，入站请求按以下逻辑路由：

```
入站协议 → selectProtocol(capabilities)
  ├── 命中 capabilities → 透传（零开销）
  └── 未命中            → 回退到 OpenAI 协议翻译
```

| 上游能力 | 下游协议 | 路由方式 |
|---------|---------|---------|
| openai + anthropic | Anthropic Messages | ✅ 透传 |
| openai | Gemini | 经 OpenAI 翻译 |
| openai + responses | Responses | ✅ 透传 |
| openai | Anthropic | 经 OpenAI 翻译 |

---

## 📐 4×4 协议互转矩阵

| 入站 ↓ \ 出站 → | OpenAI CC | Anthropic | Gemini | Responses |
|---|---|---|---|---|
| **OpenAI CC** `/v1/chat/completions` | ✅ 透传 | ✅ 翻译 | ✅ 翻译 | ✅ 翻译 |
| **Anthropic** `/v1/messages` | ✅ 翻译 | ✅ 透传 | ✅ 翻译 | ✅ 翻译 |
| **Gemini** `/v1/models/{m}:generateContent` | ✅ 翻译 | ✅ 翻译 | ✅ 透传 | ✅ 翻译 |
| **Responses** `/v1/responses` | ✅ 翻译 | ✅ 翻译 | ✅ 翻译 | ✅ 透传 |

---

## 🛠️ 编译

```bash
# 本地编译
go build -o agent-proxy .

# 跨平台
GOOS=linux   GOARCH=amd64 go build -o agent-proxy-linux-amd64 .
GOOS=darwin  GOARCH=arm64 go build -o agent-proxy-darwin-arm64 .
```

> `-ldflags="-s -w"` 可剥离调试信息，减小二进制体积。

---

## 📊 Web UI

复杂模式启动后访问 `http://localhost:8080/ui`：

- **指标卡片**：QPS / P99 延迟 / 错误率 / 活跃连接
- **Provider 列表**：健康状态圆点（🟢🟡🔴⚪）
- **实时图表**：60 秒 QPS + 延迟趋势
- **请求日志**：SSE 推送

---

## 📁 项目结构

```
agent-proxy/
├── main.go                    # 入口 & 命令行分派
├── cmd/cli.go                 # DB 管理实现
├── config.example.json        # 配置文件示例
├── README.md                  # 本文件
├── MANUAL.md                  # 完整用户手册
└── internal/
    ├── config/                # 配置结构
    ├── db/                    # SQLite 持久化
    ├── protocol/              # 协议翻译器
    │   ├── schema/            # Central Schema（中枢）
    │   ├── chatcompletion/
    │   ├── anthropic/
    │   ├── gemini/
    │   └── responses/
    ├── provider/              # 下游客户端
    ├── router/                # 模型路由
    ├── server/                # 网关服务
    ├── translator/            # 翻译接口定义
    └── web/                   # Web UI
```

---

## 📖 文档

| 文档 | 说明 |
|------|------|
| [README.md](README.md) | 快速概览、上手、常用命令 |
| [MANUAL.md](MANUAL.md) | 完整用户手册（含协议兼容性、Web UI、扩展开发） |

---

## License

MIT
# agent-proxy — AI 协议网关

## 一句话

agent-proxy 是 AI 协议领域的瑞士军刀：单二进制，把 OpenAI / Anthropic / Gemini / Responses 四种 API 格式互相转换，让任何 agent 能用任何上游模型。

## 核心能力

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
| 🧩 **模型别名映射** | 64 个内置假模型名（o1 / gpt-5 / claude-* 等）+ 动态 `@default` 上游路由 |
| 🔍 **四向 -vv 日志** | 入站 / 上游请求 / 上游响应 / 出站响应全链路查看 |

---

## 4×4 协议互转矩阵

| 入站 ↓ \ 出站 → | OpenAI CC | Anthropic | Gemini | Responses |
|---|---|---|---|---|
| **OpenAI CC** `/v1/chat/completions` | ✅ 透传 | ✅ 翻译 | ✅ 翻译 | ✅ 翻译 |
| **Anthropic** `/v1/messages` | ✅ 翻译 | ✅ 透传 | ✅ 翻译 | ✅ 翻译 |
| **Gemini** `/v1/models/{m}:generateContent` | ✅ 翻译 | ✅ 翻译 | ✅ 透传 | ✅ 翻译 |
| **Responses** `/v1/responses` | ✅ 翻译 | ✅ 翻译 | ✅ 翻译 | ✅ 透传 |

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

启动后网关同时暴露 4 种协议入口（示例省略认证头）：

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

## 🧩 模型别名映射

agent-proxy 内置一套假模型名到真实上游模型的映射机制，让你可以在任何 agent 里直接用 `o1`、`gpt-5`、`claude-opus-4-5` 等常见名字，而无需上游真的提供这些模型。

### 映射算法

| 优先级 | 来源 | 行为 |
|--------|------|------|
| 1 | `--aliases <file>` CLI 参数 | 显式指定映射文件，优先级最高 |
| 2 | 同目录 `model-aliases.yaml` | 自动检测可执行文件同目录下的映射文件 |
| 3 | 代码内置 `DefaultAliases()` | 64 个常见假模型名 + `_default_=@default` 兜底 |

映射值支持三种语法：

- `@default` — 动态取上游 `/v1/models` 返回的第一个模型，换源自动适配
- `@db:<id>,<model>` — 指定 DB 记录对应的模型
- `纯字符串` — 直接作为上游模型名

`_default_` 是一个特殊 key：所有未单独配置具体目标的别名统一走 `_default_` 的值。

### 内置假模型名（64 个）

涵盖 Claude / Codex / GPT / o 系列 / DeepSeek / Gemini / Qwen / Doubao / GLM 等主流品牌，包括 `vmodel` 自定义占位。

### 自定义映射示例

在 `model-aliases.yaml` 中：

```yaml
# 所有别名统一走 sensenova-6.7-flash-lite
_default_:             sensenova-6.7-flash-lite

# 单独指定：claude-opus-4-5 走 deepseek-v4-flash
claude-opus-4-5:       deepseek-v4-flash

# 动态取上游第一个模型
my-custom-model:       @default
```

> 样板文件 `model-aliases.yaml` 已包含上述配置，默认全部注释；取消注释并修改值即可启用。

---

## 🔍 -v / -vv 日志

快速模式支持两级详细日志：

| 级别 | 参数 | 输出内容 |
|------|------|---------|
| 基础 | `-v` | 客户端 IP / 协议 / 模型 / 状态码 / 耗时 / Token 用量 |
| 四向 | `-vv` | `-v` 基础上依次打印四条消息体 |

四向日志顺序：

```
[Guest → 代理]  入站原始请求体（客户端假模型名）
[代理 → LLM]    上游请求体（已替换为真实模型名）
[LLM → 代理]    上游原始响应体
[代理 → Guest]  出站响应体（已回显客户端模型名）
```

> body 超过 20 KB 时仅截取前 20 KB 并附省略标记，避免撑爆终端。

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
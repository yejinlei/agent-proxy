# agent-proxy 用户手册

***

## 功能概览

- **4×4 协议互转**：OpenAI Compatible / Anthropic Messages / Google Gemini / OpenAI Responses — 任意入站可转任意出站，协议匹配时零开销透传
- **模型别名映射**：三层加载策略（CLI `--aliases` > 同目录 `model-aliases.yaml` > 代码内置 `DefaultAliases()`），64 个内置假模型名 + `_default_=@default` 动态上游路由，支持 `@db:<id>,<model>` 引用 DB 记录
- **双向模型名替换**：请求体 `model` 字段（假模型 → 真实上游模型）+ 响应体 / SSE 每行（真实模型 → 回显客户端假模型名），客户端始终无感知
- **单二进制**：纯 Go 编译，内嵌 SQLite（`modernc.org/sqlite`），无外部依赖、无 CGO
- **两种启动模式**：快速模式（`run --db`，从 SQLite 选一条记录立即启动）+ 复杂模式（`run --mode complex`，多 Provider + 路由 + Web UI）
- **智能嗅探**：`db add` / `detect` 自动探测上游 4 种协议能力和模型列表，交互确认后入库
- **快速模式客户端认证**：三选一（随机生成 48 位 hex / `--key` 固定 / `--nokey` 免密钥），`/v1/models` 额外支持 `?key=<clientKey>` URL 参数，便于浏览器直接访问
- **两级日志**：`-v` 摘要（IP / 协议 / 模型 / 状态 / 耗时 / Token），`-vv` 四向全链路（入站 / 上游请求 / 上游响应 / 出站）
- **Web UI**：复杂模式 `/ui`，深色主题面板 — 指标卡片、Provider 状态圆点、60 秒 QPS + 延迟趋势图、SSE 实时请求日志

***

## 安装

### 方式一：使用编译好的可执行文件

从 GitHub Release 下载对应平台的裸二进制（不打包 ZIP），直接运行：

```powershell
.\agent-proxy.exe --help
```

每个 Release 附带 `sha256sums.txt`，下载后建议校验：

```powershell
Get-FileHash .\agent-proxy-windows-amd64.exe -Algorithm SHA256
# 与 sha256sums.txt 中对应条目对比
```

### 方式二：从源码编译

```powershell
go mod download
go build -trimpath -ldflags "-s -w" -o agent-proxy.exe .
```

依赖：

| 包 | 用途 |
|----|------|
| `github.com/go-chi/chi/v5` | HTTP 路由 |
| `modernc.org/sqlite` | 纯 Go SQLite（无需 CGO / 系统库） |

跨平台编译参考 README 的「编译」章节。

***

## 快速模式使用

### 30 秒上手

```powershell
# ① 嗅探并添加代理（自动探测所有协议和模型，交互确认入库）
agent-proxy db add --url https://token.sensenova.cn/v1 ^
                   --key sk-xxx ^
                   --name sensenova

# ② 快速模式启动（--nokey 本地开发，无需客户端认证）
agent-proxy run --db 1 --nokey -v

# ③ 任意协议调用（启动后 4 种协议入口同时可用）
# 3a. OpenAI Compatible — 直接写假模型名 o1，自动映射到上游真实模型
curl -X POST http://localhost:8080/v1/chat/completions ^
  -H "Content-Type: application/json" ^
  -d '{"model":"o1","messages":[{"role":"user","content":"hello"}]}'

# 3b. Anthropic Messages — gpt-5 同样是假模型名
curl -X POST http://localhost:8080/v1/messages ^
  -H "Content-Type: application/json" ^
  -H "x-api-key: dummy" ^
  -d '{"model":"gpt-5","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}'

# 3c. Google Gemini — vmodel 是内置占位假模型
curl -X POST "http://localhost:8080/v1/models/vmodel:generateContent" ^
  -H "Content-Type: application/json" ^
  -d '{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}'

# 3d. OpenAI Responses
curl -X POST http://localhost:8080/v1/responses ^
  -H "Content-Type: application/json" ^
  -d '{"model":"claude-sonnet-5","input":[{"type":"message","role":"user","content":"hello"}]}'
```

> 以上 `o1` / `gpt-5` / `vmodel` / `claude-sonnet-5` 都是假模型名，agent-proxy 根据别名映射算法自动替换为上游真实模型。

### 数据库存储

数据库文件位于 `~/.agent-proxy/proxies.db`（SQLite 单文件），含多协议嗅探列：

```sql
CREATE TABLE proxies (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  name              TEXT NOT NULL DEFAULT '',
  url               TEXT NOT NULL,
  key               TEXT NOT NULL,
  provider_type     TEXT NOT NULL DEFAULT 'openai',
  detected_format   TEXT,
  openai_cap        INTEGER NOT NULL DEFAULT 0,
  anthropic_cap     INTEGER NOT NULL DEFAULT 0,
  model_count       INTEGER NOT NULL DEFAULT 0,
  models_json       TEXT,
  capabilities_json TEXT,   -- 嗅探到的协议 ["openai","anthropic","gemini","responses"]
  models_map_json   TEXT,   -- 每协议模型 {"openai":[...],"anthropic":[...]}
  weight            INTEGER NOT NULL DEFAULT 100,
  created_at        TEXT NOT NULL
);
```

| 平台 | 数据库路径 |
|------|-----------|
| Windows | `%USERPROFILE%\.agent-proxy\proxies.db` |
| macOS / Linux | `~/.agent-proxy/proxies.db` |

> **安全提示**：Key 以明文存储在 SQLite 中。不要将 `.db` 文件上传到公开仓库。

### 多 Provider 说明

快速模式只支持从 DB 选**一条记录**启动。如需多 Provider 调度、路由前缀匹配、Web UI 实时监控，请使用复杂模式。

***

## 复杂模式使用

### 默认配置启动

```powershell
# 设置主代理 Key（复杂模式默认从环境变量读取）
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
agent-proxy run --mode complex --host 0.0.0.0 --port 8080 --conf agent-proxy.yaml
```

配置模板参考仓库根目录下的 `agent-proxy.yaml`。

### 自定义监听地址

```powershell
agent-proxy run --mode complex --host 127.0.0.1 --port 9090
```

### 多 Provider 配置（代码级）

在 `main.go` 的 `startComplexMode()` 函数中注册：

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

### 模型路由（复杂模式）

```go
cfg.ModelRouter.ModelToProvider = map[string]string{
    "sensenova-": "sensenova",   // 前缀匹配
    "claude-":    "anthropic",
}
cfg.ModelRouter.DefaultProvider = "sensenova"  // 兜底
```

匹配逻辑：遍历路由表，**前缀匹配 → 精确匹配 → 默认 Provider**。

路由前缀访问路径：`/p/<name>/v1/chat/completions`。

***

## CLI 命令参考

### 启动

| 命令 | 说明 |
|------|------|
| `agent-proxy run --db <id>` | 快速模式（随机密钥，默认启动方式） |
| `agent-proxy run --db <id> --key <k>` | 快速模式（指定固定密钥） |
| `agent-proxy run --db <id> --nokey` | 快速模式（免密钥，本地开发用） |
| `agent-proxy run --mode simple --db 1` | 快速模式（显式指定 mode） |
| `agent-proxy run --mode complex --host 0.0.0.0 --port 8080` | 复杂模式（默认配置） |
| `agent-proxy run --mode complex --conf agent-proxy.yaml` | 复杂模式 + 配置文件 |
| `agent-proxy run --db 1 -v` / `-vv` | 追加日志级别（摘要 / 四向全链路，仅快速模式生效） |
| `agent-proxy --help` | 显示帮助 |

### db 管理

| 命令 | 示例 | 说明 |
|------|------|------|
| `db query` | `agent-proxy db query` | 列出所有代理（id / 名称 / URL / 能力 / 模型数） |
| `db query` | `agent-proxy db query 1` | 查看 ID=1 详情（含完整协议能力和模型列表） |
| `db find` | `agent-proxy db find sensenova` | 关键词搜索（匹配名称 / URL / 协议 / 模型） |
| `db add` | `agent-proxy db add --url <u> --key <k> [--name <n>]` | 嗅探并新增（4 协议自动探测，交互确认入库） |
| `detect` | `agent-proxy detect --url <u> --key <k>` | `db add` 的兼容别名 |
| `db check` | `agent-proxy db check` | 核对所有代理有效性（重新嗅探，提示删除失效记录） |
| `db rm` | `agent-proxy db rm 1` | 删除指定记录 |

### `db add` / `detect` 参数详解

| 参数 | 默认值 | 必填 | 说明 |
|------|--------|------|------|
| `--url` | — | ✅ | 上游 API 地址，如 `https://token.sensenova.cn/v1`（不带尾斜杠，不要含重复 `/v1`） |
| `--key` | — | ✅ | 上游 API Key（存 DB 明文，注意安全） |
| `--name` | URL 值 | 否 | 友好名称，用于 `db query` 显示 |

`--type` 参数已废弃，协议由嗅探自动确定。

**嗅探步骤**：

1. `GET {url}/v1/models` + `Authorization: Bearer <key>` — 探测 OpenAI 协议和模型列表
2. `POST {url}/v1/responses` + `Authorization: Bearer <key>` — 独立探测 Responses 协议
3. `POST {url}/v1/messages` + `x-api-key: <key>` — 探测 Anthropic 协议
4. `POST {url}/v1/models/gemini-pro:generateContent` — 探测 Gemini 协议
5. 至少 1 个协议命中有模型 → 交互提示确认 → 写入 SQLite
6. 0 模型 → 直接报告失败，不提示

### `db query <id>` 输出示例

```
代理配置详情:
  ID:           1
  Name:         sensenova
  URL:          https://token.sensenova.cn/v1
  Key:          sk-abc***xyz
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

***

## 模型别名映射

### 三层加载策略

| 优先级 | 来源 | 行为 |
|--------|------|------|
| 1 | `--aliases <file>` | CLI 显式指定映射文件，优先级最高 |
| 2 | 可执行文件同目录 `model-aliases.yaml` | 启动时自动检测并加载 |
| 3 | 代码内置 `DefaultAliases()` | 64 个常见假模型名 + `_default_=@default` 兜底 |

**合并规则**：高层存在时，**以内置 64 个别名为基础**，用高层条目覆盖。这样用户只需在 `model-aliases.yaml` 配 `_default_` 或少量别名，内置别名仍然生效。

### 映射值语法

| 语法 | 说明 |
|------|------|
| `@default` | 动态取上游 `/v1/models` 返回的第一个模型，换源自动适配 |
| `@db:<id>,<model>` | 指定 DB 记录编号对应的模型 |
| 纯字符串（如 `deepseek-v4-flash`） | 直接作为上游模型名使用 |

`_default_` 是特殊 key：所有未单独配置具体目标的别名，统一走 `_default_` 的值。

### 内置假模型名（64 个）

涵盖品牌：Claude（16 个）/ Codex（3 个）/ GPT + o 系列（17 个）/ DeepSeek（3 个）/ Gemini（9 个）/ Qwen（10 个）/ Doubao（2 个）/ GLM（3 个）/ SenseNova（2 个），外加 `vmodel` 自定义占位。

### 自定义映射示例（`model-aliases.yaml`）

```yaml
# ============ agent-proxy 模型别名映射样板 ============
#
# 加载优先级（高→低）：--aliases 指定文件 > 同目录 model-aliases.yaml > 代码内置 DefaultAliases()
# 合并规则：以内置 64 个别名为基础，用当前文件条目覆盖。
#
# _default_ 是特殊 key：所有未单独写目标的别名，统一走 _default_ 的值。
# 语法：
#   @default                                动态取上游 /v1/models 第 1 个
#   @db:<id>,<model>                        指定 DB 记录的某个模型
#   deepseek-ai/deepseek-v4-flash-0731      直接写上游模型名

# _default_:             deepseek-ai/deepseek-v4-flash-0731
# claude-opus-4-5:       sensenova-6.7-flash-lite
# my-custom-model:       @default
```

### 查看映射关系

启动后访问 `/v1/models`（JSON）或 `/v1/models?simple=1`（纯文本，便于浏览器查看）。`/v1/models` 同时支持 `?key=<clientKey>` URL 参数和 `Authorization: Bearer` 头。

```
=== 上游模型 ===
sensenova-6.7-flash-lite
deepseek-v4-flash
glm-5.2
sensenova-u1-fast

=== 别名映射 ===
o1                 <-> deepseek-ai/deepseek-v4-flash-0731
gpt-5              <-> deepseek-ai/deepseek-v4-flash-0731
vmodel             <-> deepseek-ai/deepseek-v4-flash-0731
claude-opus-4-5    <-> sensenova-6.7-flash-lite
...
```

### 四向模型名替换

别名映射**不仅仅改 URL 路径**，还作用于整个四向消息体 JSON：

| 方向 | 替换 | 作用位置 |
|------|------|---------|
| 入站 → 上游（请求） | 假模型名 → 真实上游模型名 | 请求体 JSON 的 `model` 字段（透传非流式 / 流式 / 翻译链路 `internalReq.Model` 均覆盖） |
| 上游 → 客户端（响应） | 真实模型名 → 客户端原始假模型名 | 非流式响应体 `model` 字段；流式 SSE 每行 `model` 字段 |

客户端始终看到自己传入的假模型名，上游始终收到真实模型名，中间全链路自动替换。`@default` 首次解析时通过 `ensureModels` 懒加载上游 `/v1/models`，双重检查锁保证并发安全。

***

## 客户端认证

快速模式默认要求客户端通过 `Authorization: Bearer <key>` 头认证。三种使用方式：

### 方式一：随机生成密钥（默认）

不传 `--key` 也不传 `--nokey`，启动时自动随机生成 48 位 hex 密钥并打印到控制台：

```powershell
agent-proxy run --db 1
```

```
🚀 Agent-Proxy (快速模式) running on http://127.0.0.1:8080
🔑 Proxy Key:      sk-a1b2c3d4e5f67890abcdef1234567890abcdef12...
🔐 客户端需使用 Authorization: Bearer sk-a1b2c3d4e5f6... 连接
```

客户端调用：

```powershell
curl -X POST http://localhost:8080/v1/chat/completions ^
  -H "Authorization: Bearer sk-a1b2c3d4e5f6..." ^
  -H "Content-Type: application/json" ^
  -d '{"model":"o1","messages":[{"role":"user","content":"hi"}]}'
```

> 密钥每次重启都会变化，仅适合本地临时使用。

### 方式二：指定固定密钥

```powershell
agent-proxy run --db 1 --key sk-my-fixed-key
```

客户端连接时使用同一个 `sk-my-fixed-key`。适合自动化脚本 / 固定环境。

### 方式三：无需密钥（本地开发用）

```powershell
agent-proxy run --db 1 --nokey
```

任何客户端均可直接连接，**不要在生产环境使用此模式**。

### 认证行为说明

| 启动标志 | 密钥来源 | 客户端需 `Authorization` 头 | 适用场景 |
|----------|---------|---------------------------|---------|
| （不传） | 每次启动随机 48 位 hex | ✅ | 本地临时使用 |
| `--key sk-xxx` | 用户指定固定值 | ✅ | 自动化脚本 / 固定环境 |
| `--nokey` | 无 | ❌ | 本地开发、内网直连 |

- 缺密钥或密钥错误 → `401 Unauthorized`，标准错误格式（`error.type` / `error.message`）
- **`/health` 端点始终不受认证影响**，可用于健康检查 / LB 探针
- **`/v1/models` 额外支持 `?key=<clientKey>` URL 参数**，与 `Authorization` 头等效，便于浏览器直接访问
- 未认证的请求不会到达下游 Provider

***

## 协议兼容性

agent-proxy 支持 4×4 协议互转（OpenAI / Anthropic / Gemini / Responses），入站协议与上游协议相同时零开销透传，不同时自动翻译。

### 流式策略

无命令行开关。网关按请求体 `stream` 字段自适应路由：请求带 `stream: true` 走 SSE 流式，否则走非流式 JSON；上游返回的非流式响应会按需包装成 SSE。该策略自 v0.2.60 起取代已移除的 `--stream-mode` 参数。

> 底层机制（Central Schema 架构、透传/翻译路径、心跳机制、SSE 合规性、Anthropic 事件生命周期、消息转换流转图、Claude Code 接入流程等）详见 [设计文档](docs/DESIGN.md)。

***

## -v / -vv 日志

`-v` 仅快速模式生效；复杂模式下 `-v` 为 no-op，需用 `-vv` 才输出请求/响应体。作为启动标志追加即可。

### 日志级别

| 级别 | 参数 | 输出内容 |
|------|------|---------|
| 默认 | （不传） | 仅启动信息 + 错误日志 |
| 摘要 | `-v` | 客户端 IP / 入站协议 / 上游协议 / 模型 / 状态码 / 耗时 / Token 用量（仅快速模式） |
| 四向 | `-vv` | `-v` 基础上，依次打印四条完整消息体（两种模式均生效） |

### 摘要日志（-v）示例

```
[请求 192.168.1.10]  上游: https://token.sensenova.cn
  协议: OpenAI → openai  |  模型: sensenova-6.7-flash-lite
  状态: 200  |  耗时: 3333ms  |  Token: 128/2048
```

### 四向日志（-vv）顺序

每条请求按顺序依次打印：

```
[Guest → 代理]  入站原始请求体
  — 保留客户端传入的假模型名（o1 / vmodel / gpt-5 等）

[代理 → LLM]    上游请求体
  — model 已替换为解析后的真实上游模型名
  — 若为翻译链路，body 是下游协议格式；若为透传，body 与入站格式一致但 model 已替换

[LLM → 代理]    上游原始响应体
  — 保留上游返回的真实模型名

[代理 → Guest]  出站响应体
  — model 已回显为客户端原始假模型名
  — 流式透传：SSE 每行独立回显
```

### body 截断规则

`formatJSON` 逻辑：
- **< 20 KB**：完整格式化打印（缩进）
- **≥ 20 KB**：截取前 20 KB + `\n... (body too large)` 标记，避免撑爆终端

***

## Web UI 使用

### 访问

复杂模式启动后，浏览器打开：

```
http://localhost:8080/ui
```

### 功能区域

| 区域 | 内容 | 更新方式 |
|------|------|---------|
| 顶部指标卡 | QPS / P99 延迟 / 错误率 / 活跃连接数 | 2 秒轮询 |
| Provider 列表 | 名称 / 状态圆点 / 请求数 / 平均延迟 | 2 秒轮询 |
| QPS 图表 | 最近 60 秒 QPS 趋势 | SSE 推送 |
| 延迟图表 | 最近 60 秒延迟趋势 | SSE 推送 |
| 请求日志 | 时间 / 模型 / Provider / 状态码 / 耗时 | SSE 推送 |

### Provider 状态

| 颜色 | 含义 |
|------|------|
| 绿 | 健康（最近有成功请求） |
| 黄 | 降级（部分请求失败） |
| 红 | 宕机（最近全部失败） |
| 白 | 空闲（无请求） |

### API 端点

| 端点 | 方法 | 格式 | 说明 |
|------|------|------|------|
| `/ui/api/summary` | GET | JSON | 全量指标快照（指标卡 + Provider 列表） |
| `/ui/api/logs` | GET | SSE | 实时请求日志流 |
| `/ui/api/metrics` | GET | SSE | 实时 QPS + 延迟指标流 |
| `/ui/api/providers` | GET | JSON | Provider 状态列表（含健康度） |

***

## 高级配置

### 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `AGENT_PROXY_API_KEY` | — | 复杂模式主代理 API Key |
| `ANTHROPIC_API_KEY` | — | Anthropic Provider 专用 Key |

### 限流配置

```go
type RateLimitConfig struct {
    RequestsPerSecond int `yaml:"requests_per_second"`
    Burst             int `yaml:"burst"`
}
```

令牌桶算法，默认 100 QPS / 200 burst。

### Provider 超时

```go
type ProviderConfig struct {
    TimeoutSec int `yaml:"timeout_sec"`    // 单次调用超时（秒）
    RateLimit  int `yaml:"rate_limit"`     // 本地限流
}
```

### 数据库位置 / 迁移

数据库是 SQLite 单文件，直接复制 `~/.agent-proxy/proxies.db` 即可跨机器迁移。删除该文件等同于重置所有代理记录。

***

## 故障排查

### 问题 1：启动后无法连接上游 Provider

```
症状：所有请求返回 401 / 404 / 超时
排查：
  1. 确认 URL 和 Key 是否正确
  2. 用 db check 重新嗅探并核对有效性
  3. 运行 curl -H "Authorization: Bearer <key>" <url>/v1/models 直连验证
```

### 问题 2：URL 404（/v1/v1/... 重复路径）

```
原因：proxyBaseURL 末尾带 /v1，请求时 BuildURL 又追加了一次
解决：DB 中记录的 URL 去掉末尾 /v1，确保是 https://token.sensenova.cn 而不是 https://token.sensenova.cn/v1
```

### 问题 3：快速模式报错「未找到 ID」

```
原因：DB 为空或 ID 不存在
解决：
  agent-proxy db query   # 查看现有记录
  agent-proxy db add     # 添加新记录
```

### 问题 4：端口被占用

```
原因：另一个 agent-proxy 实例仍在运行
解决：
  netstat -ano | Select-String ":8080"
  taskkill /F /PID <pid>
  # 或启动时指定其他端口
  agent-proxy run --db 1 --port 9090
```

### 问题 5：流式请求卡住

```
原因：下游无 SSE 响应或连接超时
解决：
  1. 降低 Provider TimeoutSec
  2. 检查网络连通性（上游 /v1/models 直连）
  3. 确认下游支持 stream=true（部分网关需显式开启）
```

### 问题 6：假模型名报错「模型不存在」

```
症状：客户端传 o1 / vmodel 等，上游返回 model not found
排查：
  1. 确认请求体 JSON 的 model 字段是否被正确替换（加 -vv 看 [代理→LLM] 一行）
  2. 若仍为假模型名：检查是否是复杂模式（复杂模式暂未集成别名映射，需切换到快速模式）
  3. 查看 /v1/models?simple=1 的 === 别名映射 === 是否有对应条目
  4. 若全部为空：确认 model-aliases.yaml 是否被正确加载（启动日志搜索 "merged" / "using built-in"）
```

### 问题 7：/v1/models 401 Unauthorized

```
原因：客户端认证未通过
解决：
  - Authorization: Bearer <clientKey> 头
  - 或 GET /v1/models?key=<clientKey>（浏览器访问推荐）
```

***

## 扩展开发

新增协议、翻译器、假模型名等开发指南见 [设计文档](docs/DESIGN.md)。

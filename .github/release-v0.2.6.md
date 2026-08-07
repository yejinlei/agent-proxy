# v0.2.6 — 快速模式客户端认证

**快速模式（`--mode simple` / `--db <id>`）新增客户端认证，默认随机生成密钥，可选固定密钥或无认证。**

## 新增

- **客户端认证** — 快速模式支持三种认证模式，通过 `Authorization: Bearer <key>` 头控制客户端连接：
  - **默认（不传 `--key` 也不传 `--nokey`）**：启动时随机生成 48 位 hex 密钥并打印到控制台，客户端必须使用
  - **`--key <k>`**：指定固定密钥，客户端使用同一密钥连接
  - **`--nokey`**：无需密钥，任意客户端可直接连接（仅用于本地开发）
- **随机密钥生成** — 基于 `crypto/rand` 生成 24 字节随机 hex，前缀 `sk-`
- **`/health` 端点始终免认证** — 可用于健康检查与负载均衡探测

## Bug 修复

- **认证错误返回格式** — `middleware.Auth()` 现返回与网关一致的标准 JSON 错误格式
  (`{"error":{"type":"invalid_request_error","message":"invalid api key","code":"401"}}`)，
  而非纯文本 `Unauthorized`，避免解析 `error.type` 的客户端出错
- **控制台密钥行排版** — 去除 `Proxy Key:` 行多余对齐空格，脚本化提取密钥
  (`grep "Proxy Key:" | sed 's/.*: //'`) 时不会捕获前导空白

## 用法

```powershell
# 默认随机密钥
agent-proxy run --db 1

# 固定密钥
agent-proxy run --db 1 --key sk-my-fixed-key

# 无需密钥（本地开发）
agent-proxy run --db 1 --nokey
```

## 认证行为

| 标志 | 密钥来源 | 客户端是否需要 `Authorization` 头 | 适用场景 |
|------|---------|----------------------------------|---------|
| （不传） | 随机生成 | ✅ 需要 | 本地临时使用、快速试用 |
| `--key <k>` | 指定值 | ✅ 需要 | 固定环境、自动化脚本 |
| `--nokey` | 无 | ❌ 不需要 | 本地开发、内网直连 |

- 缺密钥或密钥错误时返回 `401 Unauthorized`，响应体为标准 JSON 错误格式（`error.type`, `error.message`, `error.code`）
- `/health` 端点不受认证影响
- 未认证请求被拒绝，不会到达下游 Provider
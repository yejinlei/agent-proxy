# v0.2.140

## 变更：响应头透传全量过滤 + 回归测试锁定

一次**正确性修复**。修的是 `IncompleteRead` —— 非流式客户端按 `Content-Length` 读体时读到一半被截断。

### 问题

2026-09-14 实测同一台部署的 v0.2.139，`POST /v1/responses` 非流式 **2/9 请求 CL 错位**：

| 请求 | Content-Length 头 | 实际写入字节 |
|------|------------------|-------------|
| 短回复 ×7 | 300 / 300 / **1215** / 300 / 300 / 300 | 全部 300 |
| 长回复 ×3 | 455 / 455 / **1729** | 全部 455 |

错位时客户端报 `http.client.IncompleteRead`：头说 1215 字节，实际只有 300，连接已关闭 → 读不够。

### 根因

代理翻译会**大幅缩短**响应体。用 verbose 模式量到的一条真实链路：

```
上游响应体  6016 字节   （含 reasoning_content，1231 个 reasoning_tokens）
出站响应体    302 字节   （reasoning 被剥掉，只剩 content:"我在"）
```

差了 20 倍。上游那个 `Content-Length` 是按上游自己的体算的，代理写出的是翻译后的短体，
两个长度一起进 header map 就必然错位。Go 的 `net/http` 行为是：`Content-Length` 已在
header map 里就**照发不校验**，没有才按实际体算。

「7/9 正确」不是侥幸 —— 是上游碰巧用了 chunked（不带 CL），Go 见 map 里没有 CL 才自己算。
观测到的 CL 只有 300 / 455 / 1215 / 1729 四种，而实际恒为 300 / 455；1215−300=915、
1729−455=1274 正好是中文 reasoning 的额外字节。上游是否附带 CL 本身也间歇，所以错位间歇。

**修复后不再依赖上游行为**：过滤掉上游 CL，两种情况都由 Go 按实际体重算，恒正确。

### 修复的 6 个转发点

过滤规则早就有了（`isConnectionManagementHeader`），但 10 个转发点里只有 4 个在用。
v0.2.140 补齐剩余 6 处：

| 文件:行 | 路径 |
|---------|------|
| `quick.go:1323` | 透传流式（带 body） |
| `quick.go:1700` | 透传流式 |
| `quick.go:2685` | 翻译路径 非流式→JSON |
| `quick.go:2840` | 翻译路径 非流式→SSE |
| `quick.go:3611` | `/v1/models` 别名注入分支 |
| `gateway.go:472` | 透传非流式 |

`quick.go:3611` 是这次新发现的第 6 个，`handleModels` 里有**两个**转发点：
上方 `?simple=1` 之前那个早已过滤，别名注入这个没有。它遍历的是 `resp.Header` 而不是
`headers`，原 AST 测试只扫 `headers`，所以完全看不到 —— 测试也一并扩到了两种形态。

### 为什么用 AST 测试锁定

这 6 个循环每个只有 6 行，靠行号锚点会随任何无关改动失效。用 AST 匹配的是语义
（遍历响应头的 `RangeStmt`），跟格式无关。新增转发点时忘了加过滤，测试直接失败并打印
`文件:行号`。

这个项目的失败模式历来是「修 A 坏 B」，所以这次不只是补 6 处，而是把**以后新增的第 11 处**
也锁住了。

```
--- PASS: TestNoUnfilteredHeaderForwarding
    header_forward_test.go:85: quick.go:   7 个响应头转发点全部带 hop-by-hop 过滤
    header_forward_test.go:85: gateway.go: 3 个响应头转发点全部带 hop-by-hop 过滤
```

### 变更文件

- `main.go`：版本 `v0.2.139` → `v0.2.140`
- `internal/server/quick.go`：5 处转发点补 `isConnectionManagementHeader` 过滤
  + `@AI_GUARD: STREAM_RESPONSE_HEADERS` ×2 / `NONSTREAM_RESPONSE_HEADERS` /
  `NONSTREAM_A2S_HEADERS` / `MODELS_ALIASES_HEADERS`
- `internal/server/gateway.go`：1 处 + `@AI_GUARD: GATEWAY_NONSTREAM_RESPONSE_HEADERS`
- `internal/server/header_forward_test.go`：**新增**
  - `TestIsConnectionManagementHeader` 锁住过滤集合（7 个必丢 + 大小写 + 6 个必留）
  - `TestNoUnfilteredHeaderForwarding` AST 扫 `range headers` **和** `range resp.Header`
- `CLAUDE.md` / `AGENTS.md`：guard 表新增 4 项（83 项 / 199 处标记），保持逐字同步

`internal/protocol/*` 未改动。6 处都是 `server` 包内下游响应头透传，与协议翻译器无关，
也不涉及 quick.go / gateway.go 双模式同步（`handleModels` 只在 quick.go）。

### 验证

```bash
go test ./internal/server/          # 全绿（20.2s）
go vet  ./internal/server/ ...      # 干净
```

修复后实测同一台服务器的同一组请求，`Content-Length: 302` = 实际 302 字节，一致。

### 已知局限

部署版 v0.2.139 上该问题仍在真实存在（2/9 可复现），本 release 的二进制需要重新部署才生效。
SSH 不可用，无法由本仓库侧完成部署 —— 需要到服务器上重新构建并替换。

## ⚠️ tag 指向说明（源码可复现性）

本 release 的 tag `v0.2.140` 指向提交 `65b31c3`，**该提交不含本次改动**。

`65b31c3` 是 v0.2.139 时代的提交（`docs: release-v0.2.139.md 末尾补 tag 指向说明`），
它的 `main.go` 仍是 `v0.2.139`，6 个响应头转发点也都没有过滤。
`git clone --branch v0.2.140` 会拿到 v0.2.139 的源码。

**源码可复现性走 `master`**：本次改动的提交 `91edad8` 已推送到远端 `master`
（`65b31c3..91edad8`），`git log master` 可见：

```
91edad8  fix: 响应头透传全量过滤 Content-Length，修复非流式 IncompleteRead
        8 files changed, 359 insertions(+), 8 deletions(-)
```

包含 `main.go`（v0.2.140）、`internal/server/quick.go`（5 处过滤）、
`internal/server/gateway.go`（1 处过滤）、`internal/server/header_forward_test.go`
（新增，AST 锁 10 个转发点）、`build.ps1`、`docs/release-v0.2.140.md`、
`CLAUDE.md` / `AGENTS.md`。8 个文件全是源码/文档/脚本，**无二进制入库**
（`git ls-files | grep -cE 'agent-proxy_(linux|windows|darwin)'` = 0）；
二进制资产经 `gh release upload` 发布，不占 git 历史。

**要复现本 release 的源码，请 `git checkout master`，不要用 tag。**

### 为什么 tag 指错

`gh release create --target master` 会把 tag 落在远端 `master` 指向的 commit 上。
发 release 时 `github.com:443` 持续不可达（`Failed to connect`，重试 10 次全失败），
而 `api.github.com:443` 是通的（`gh` 全程可用）—— 两个域名走不同网络路径，
所以 `gh api` / `gh release` 能用，但 `git push origin master` 一直推不上去。
远端 `master` 因此停在 `65b31c3`，tag 跟着落在了那里。

网络恢复后 `master` 已补推成功（`git push origin master:master` → `65b31c3..91edad8`）。
**tag 仍指旧 commit**：修正需要 `git push --force --tags origin v0.2.140`，
而该操作会把 tag 从当前锚点移走、旧 tag 对象不再可达，属不可逆动作，
未获显式授权故保留原状，在此说明。

与 v0.2.139 是同一个坑（v0.2.139 的 tag 也落在了错误 commit 上）。

### 资产本身是正确的

7 个二进制都是用 `-ldflags "-X main.version=v0.2.140"` 从含改动的源码编译的。
验证方式：

```bash
strings agent-proxy_linux_amd64 | grep -x 'v0.2.140'
```

本 release 实测：远端下载的 `agent-proxy_linux_amd64` 为 13,750,456 字节，
含 `v0.2.140` 字符串 2 次、`v0.2.139` 0 次，函数符号
`QuickGateway).handleModels` / `handleNonStreamResponse` 均在。
（哈希比对因 objects 域名间歇不可达未完成 —— 该域名在本环境下载不稳定，
首次下载曾截断为 13,115,267 字节。）

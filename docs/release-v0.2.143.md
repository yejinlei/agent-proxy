# v0.2.143

## 变更：入站请求体上限 1MiB → 16MiB，可配置

一次**故障修复**。修的是 Codex「停住」的直接原因 —— HTTPS 入站 1MiB 硬上限被
全量重发历史打穿，稳定 413。

### 现象

v0.2.142 生产环境 09-14 15 时，Codex 侧出现「turn 停住」。查
`C:\Users\T480\.codex\logs_2.sqlite` 的 `responses_retry` 事件，一小时 25 条，
分成两类：

| 类型 | 次数 | 特征 |
|------|------|------|
| `413 Payload Too Large: got 1196624 bytes, limit 1048576` | 14 条 retry（含重试共 58 行） | 集中单一 thread，5 次重试全失败 |
| `Connection closed normally`（WS 发送侧） | 11 条 | 散布 9 个 thread，各 1 次 |

413 全部集中在 `09-14 15` 这一个小时，观测到的 body 尺寸稳定在
**1,196,028 ~ 1,197,058** 字节（6 个不同值，均 1.14x 上限）。

### 为什么 413 会让 Codex 停住

`413` 不是瞬时错误。Codex 的重试预算是 `max_retries=5`，指数退避后**每次重试都
拿同一个 1.19MiB 请求体再撞一次同一个上限** —— 5 次全部以完全相同的方式失败，
重试配额耗尽，turn 不再前进一步。用户在 UI 上看到的「停住」就是这个状态。

而历史只会单调增长：Codex 每轮全量重发历史（从不发 `previous_response_id`），
所以这条线一旦越过 1MiB 就**永久越线**，直到用户开新会话。这不是偶发故障，
是任何用到图片/长上下文的会话的必然结局。

### 为什么 WS 没事、HTTPS 出事

代理有两条入站路径，两条独立上限：

- WS（`/v1/responses` 的 WebSocket）：`qwsMaxPayload` / `wsMaxPayload` = 64MiB，
  且 `handleResponsesWebSocket` **绕过** `readBody`。
- HTTPS：`maxBodyBytes` = 1MiB。

同一条 1.19MiB 的请求，走 WS 能过、走 HTTPS 就 413。所以「随机 413」的排查
第一步是确认请求走了哪条路 —— 这不是 bug，是两条路径的上限从没对齐过，
只是这次才被全量历史撞出来。

### 改动

**`internal/server/quick.go`**：`const maxBodyBytes = 1 << 20` 改为
`const defaultMaxBodyBytes = 16 << 20` + `var maxBodyBytes` +
`SetMaxBodyBytes(n)` 启动期配置入口。加 `@AI_GUARD: INBOUND_BODY_LIMIT`，
两条 CONSTRAINT 分别锁定「与 WS 64MiB 是两条独立上限」和「只允许启动期设置」。

**`internal/config/config.go`**：`server.max_body_bytes`，默认 `16 << 20`。

**`main.go`**：`--max-body-bytes <n>`，命令行优先于配置文件；启动 banner 追加
`max-body=<生效值>`，让生产环境一眼能看到当前上限。

### 为什么是 16MiB

不是贴住 1.19MiB 算余量。1.19MiB 只是**第一次撞墙时**的尺寸 —— 模型当时仍在
正常工作，历史会继续涨。贴住上限等于把撞墙时间往后推一点，不是修好。

16MiB 给 13 倍余量，同时仍是显式的防误用上限（不是把 `readBody` 删掉），
保留「客户端真发了巨大请求 → 明确 413」而不是让代理内存跟着涨。

真正治本的是历史压缩，但那是 Codex 侧行为，不在本仓可控范围内；也不改 Codex
提示词。

### 为什么 quick.go / gateway.go 不同步改

`readBody` 定义在 quick.go、同 package 共享，4 个调用点
（quick.go `handleRequest` / `handlePassthroughNonStream` / `handlePassthroughStream`
+ gateway.go `handleRequest`）都读同一个变量。改这一处即覆盖双模式，
`git diff -- internal/server/gateway.go` 为空是**正确结果**，不是漏改。

### 测试

原有 4 个 `readBody` 测试全部引用符号 `maxBodyBytes`（不写死 1048576），
因此自动跟踪新值，零测试改动仍全绿。新增：

- `TestReadBody_ProductionObservedSize`：用生产实测的 **1,196,624 字节**回归，
  必须放行。这是本事故的直接复现。
- `TestMaxBodyBytesDefault`：锁定默认 16MiB，防误改回 1MiB。
- `TestSetMaxBodyBytes`：正数生效、0 与负数回落默认。

```bash
go build ./...                                    # 干净
go test -count=1 ./...                            # 全绿
go test -count=1 -run 'ReadBody|MaxBodyBytes|SetMaxBodyBytes' -v ./internal/server/
    TestReadBody_UnderLimit                    PASS
    TestReadBody_ProductionObservedSize        PASS
    TestMaxBodyBytesDefault                    PASS
    TestSetMaxBodyBytes                        PASS
    TestReadBody_ExactlyAtLimit                PASS
    TestReadBody_OverLimit                     PASS
    TestReadBody_OverLimit_NoContentLength     PASS
    TestReadBody_ReadErrorSurfaces             PASS
```

### 端到端验证（v0.2.143 本地起服务，非单元测试）

| 请求体 | 期望 | 实测 |
|--------|------|------|
| 1,196,623 字节（生产事故尺寸） | 放行 | HTTP 401（上游 key 失效，**已过 body 检查并被转发**） |
| 15,728,639 字节（< 16MiB） | 放行 | 转发上游（上游自身 size limit） |
| 20,971,519 字节（> 16MiB） | 413 | `413 "got 20971519 bytes, limit 16777216"` |

上限仍生效、不再误杀，两个方向都验证过。

### 变更文件

- `internal/server/quick.go`：`defaultMaxBodyBytes` / `maxBodyBytes` /
  `SetMaxBodyBytes` + `@AI_GUARD: INBOUND_BODY_LIMIT`，`readBody` 两处
  `int64()` 转换
- `internal/config/config.go`：`Server.MaxBodyBytes` 字段 + 默认 16MiB
- `main.go`：`--max-body-bytes` flag、配置文件读取、banner 打印、
  版本 `v0.2.142` → `v0.2.143`
- `internal/server/gateway_test.go`：3 个新测试（生产尺寸回归 + 默认值锁定 + 配置入口）
- `CLAUDE.md` / `AGENTS.md`：guard 表新增 `INBOUND_BODY_LIMIT`，保持逐字同步

### 已知局限

- **WS 与 HTTPS 仍是不对称的两条上限**（64MiB vs 16MiB）。这次把 HTTPS 抬高到
  实用区间，但没把两者合并 —— 它们走不同代码路径，强行统一会引入不必要的
  耦合。若将来 HTTPS 又要撞墙，先确认请求该走哪条路。
- **历史压缩没做**。请求体仍会随会话增长，只是撞墙时间从「1.19MiB」推到
  「16MiB」。长会话终局仍受此限。
- **`Connection closed normally`（WS 发送侧，11 次 / 9 thread 各 1 次）未处理。**
  散布且单次，特征像 thread 拆连竞态，不像系统性 WS 故障；WS 心跳本身是健康的
  （61/61 干净、`err=<nil>`、item 绑定正确）。优先级低于 413。

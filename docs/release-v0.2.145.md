# v0.2.145

## 变更：上游静默主动断流 —— 60s 强制收尾，不再靠 300s 全局超时兜底

v0.2.144 部署后生产日志暴露出一条**挂死连接**：感诺上游接受请求后完全静默，
代理一直等到 `q.timeout`（默认 300s）才放弃。期间 goroutine + 上游 HTTP 连接
一直被占着，而 WS 心跳每 5s 发合法 delta 事件把连接养活着 —— 反而**阻止了
Codex 自己判死**，表现为 Codex 无限 working。

### 现场（agent-proxy-9091.log，v0.2.144）

```
12:05:02  WS handshake OK            ← 连接 #1
12:05:03  WS handshake OK            ← 连接 #2
12:05:03  response.created + output_item.added   （连接 #2）
12:05:17  WS frame → client FAILED: write: broken pipe   （连接 #1，Codex 主动关）
12:05:17  TranslateStream END(err)   （连接 #1，took=14.95s）
12:05:17  WS handshake OK            ← 连接 #3
12:05:25  TranslateStream END + [request] total   （连接 #3，took=7.52s）
12:05:25  —— 连接 #2 到这里再无 END、再无 total ——
12:05:25 → 12:07:58  WS heartbeat → client: err=<nil>   ×36 次，连续 2 分 50 秒
```

3 次握手、2 条 `[request] total`、1 条连接**零事件到达后一直挂着**。连接 #2 收
到 `response.created` + `output_item.added` 后上游再无任何 SSE 事件，没有
`TranslateStream END`、没有 `[request] total`。心跳全 `err=<nil>` 说明 Codex 侧
还挂着这条 WS 没关。

### 修复

上游 SSE 行 channel 加 idle 超时。连续静默超过 `upstreamStallTimeout`（60s）
就关闭输出 channel：

```
stallTimeoutChan(src, ctx, 60s, ...)
   超时 → close(out)
      → 生产者 for line := range 退出
         → defer close(events)
            → TranslateStream channel 关闭分支
               → 补发完整终止序列（output_item.done + response.completed + event:done/[DONE]）
```

用**关闭 channel** 而非 `cancel(ctx)`，因为 `ctx.Done` 分支的语义是「客户端断连」，
而这里是「上游无响应」；走 channel 关闭路径收尾序列与「上游正常结束」一致，
且保留 `TranslateStream END` 日志的区分度。

`stallTimeoutChan` 在 quick.go 定义、gateway.go 复用（双模式同步）。

### 60s 的取值依据

- 感诺冷启动首 token 实测 **2.4~12.0s**
- 单轮长推理 **10~35s**
- 60s = 正常最长静默的 2 倍

放大拉长泄漏窗口，放小误杀长推理。

### 顺带修好的另一件事

**WS 心跳不再掩盖上游死掉。** 此前心跳是「应用层信号」，Codex 看到持续的合法
delta 事件就不会自己判死连接。现在代理主动断流后，心跳随流结束一起停止
（`observeHeartbeatState` 收到 `[DONE]` 置 `hbOn=false`），Codex 能正常感知
这一轮结束。

### 变更文件

- `internal/server/quick.go`：新增 `upstreamStallTimeout` 常量 + `stallTimeoutChan`
  包装函数（`@AI_GUARD: UPSTREAM_STALL_DETECT`）
- `internal/server/gateway.go`：`handleStreamRequest` 复用同一包装（双模式同步）
- `internal/server/stall_timeout_test.go`：**新增**，3 test
- `main.go`：版本 `v0.2.144` → `v0.2.145`
- `CLAUDE.md` / `AGENTS.md`：guard 表 97 → 98 项，新增 `UPSTREAM_STALL_DETECT`，
  保持逐字同步

### 验证

```bash
go build ./...                                  # 干净
go vet ./internal/server/                       # 干净

go test ./internal/server/ -run StallTimeout -v
    TestStallTimeoutChan_ClosesOnIdle    PASS   # idle 50ms → channel 关闭
    TestStallTimeoutChan_PassesThrough   PASS   # 正常数据透传 + 上游 EOF 传播
    TestStallTimeoutChan_CtxCancel       PASS   # ctx 取消立即关闭
```

### 已知局限

- **静默判据是「上游 SSE 行 channel idle」，不是「translator 产出内容」**。上游持续
  发空 content 分片时 channel 不 idle、超时不触发，translator 侧仍会看到大量空
  delta。这是模型输出质量问题（glm-5.2 实测 59 个 delta 里 58 个空），不属
  本 guard 的判据范围。
- 上游 EOF、ctx 取消、idle 超时三种关闭对下游不可区分（都是「上游没了」），
  语义上也不需要区分；`TranslateStream END` 日志的 `http=` / `upstream_msg=`
  字段只在真正报过上游错误时才有值。

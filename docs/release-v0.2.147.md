# v0.2.147

## 变更：给非流式→SSE 路径补 60s 超时上限（修掉 v0.2.146 引入的 300s 静默）

上一版为了修大请求的 502，把翻译路径大请求降级到非流式→SSE。降级路由本身是对的，
但它指向的函数**没有任何超时界**，一次上游挂死被拉成了 5 分钟零内容。

### 1. 现场

`agent-proxy-9091.log`（v0.2.146，09-15 14:42:35，翻译路径 215939 bytes 下游体）：

```
[route] translation stream=true, large body (215939 bytes > 102400) → use non-stream→SSE
[heartbeat] started → interval=500ms
[provider] POST https://token.sensenova.cn/v1/chat/completions body_len=215940
...600 个心跳，5 分钟零内容...
[upstream] Call glm-5.2 → 5m0.000473289s
[heartbeat] stopped (total=600 beats)
[request] total → 5m0.061303983s
```

同一个进程、同一分钟内，14:42:34 的流式请求 15.005s 就返回了 3405 字符。

原因：两个非流式→SSE 包装函数都用 `context.WithTimeout(ctx, q.timeout)`（默认 300s），
而非流式调用期间上游**全程零字节**——headers 要等完整 JSON 生成完才回。流式路径的 idle
判据（`stallTimeoutChan` 包装 line channel）在这里无处挂载：没有 line，只有一个阻塞读。
所以只有 ctx 超时能判死，而它被设成了 300s。

### 2. 直连实测：排除两个误判

拿到真 key 直连 `token.sensenova.cn` 的 glm-5.2，用 Codex 同形负载
（13 个工具、65 条消息、21332 字符 instructions）跑了两组对照。

**`stream_options` 无罪。** 用户怀疑是 usage 注入那块引起的，实测不成立：

| 请求 | 结果 |
|---|---|
| 非流式，无 stream_options | 200，5.18s |
| 非流式 + stream_options | 200，41.69s |
| 流式 + stream_options | 200，3.79s |
| 非流式 + tools + stream_options | 200，2.11s |

而日志里那条挂死的请求**也带了** `stream_options:{include_usage:true}`，
14:42:34 的 prewarm 请求带同样的字段、15s 正常返回——同形请求一个活一个挂，
不是字段兼容性问题。

**「大请求必然挂」也不成立。**

| 负载 | 模式 | 结果 |
|---|---|---|
| 20000 B | 非流式 | 200，2.38s |
| 80000 B | 非流式 | 200，5.60s |
| 150000 B | 非流式 | 200，6.69s |
| 216000 B | 非流式 | 200，9.99s |
| 118KB 真实形状 | 非流式 | 200，6.55s / 7.60s（两次） |
| 799KB 真实形状 | 流式 | 200，first_byte 27.01s、完整返回 36.90s |

结论：**大请求两种模式都能完成，而且非流式明显更快。** 所以 v0.2.146 的降级路由
保留——它规避的是 v0.2.144 反复出现的 502（6 次现场），而 v0.2.144 的 502 与
v0.2.146 的挂死其实是同一批上游偶发故障的两种表现，降级路由规避不了它。
唯一能做的，是把挂死变成有界的失败。

### 3. 修复

新增包级常量 `nonStreamCallTimeout = 60 * time.Second`，两个非流式→SSE 包装函数
统一改用：

```go
callCtx, cancel := context.WithTimeout(ctx, nonStreamCallTimeout)
```

- `handleNonStreamResponseAsSSE`（翻译路径，本次事故现场）
- `handlePassthroughNonStreamAsSSE`（透传路径，同一隐患）

取 60s 与 `upstreamStallTimeout` 同值：两者都是「上游沉默多久算判死」，同一个代理
不该在流式路径 60s 收尾、非流式路径 300s 收尾。实测最慢观测值是 799KB 流式
first_byte 27s，60s 留 2 倍以上余量，只杀真正的挂死。

**没动的地方**：`handleNonStreamResponse`（raw JSON 返回）和所有流式路径仍用
`q.timeout`——前者客户端不指望 SSE 长连接，后者由 `stallTimeoutChan` 负责判死。
本次只修「已发出 SSE 头、客户端会一直等、上游却零字节」这一个具体形状。

### 4. gateway.go

复杂模式的翻译路径仍只打日志不降级，但补了一条 `@CONSTRAINT` 说明：quick.go 的
降级路径现在受 60s 约束，本路径若长期只靠 `q.timeout` 兜底，两个模式在大请求挂死时
行为会不一致。这是已知分叉，属独立工作量。

顺手修正了一处归因错误：原注释写「gateway.go 没有 handleNonStreamResponseAsSSE
包装函数」，实际本文件有 `writeNonStreamAsSSE`，只是它是写帧辅助、被流式 fallback
调用，不是可直接调的完整 handler。

### 变更文件

- `internal/server/quick.go`：新增 `nonStreamCallTimeout` 常量（`NONSTREAM_CALL_TIMEOUT`
  guard），两个非流式→SSE 包装函数改用
- `internal/server/gateway.go`：修正 `LARGE_BODY_SKIP_STREAM_TRANSLATION` 的归因注释
- `internal/server/nonstream_call_timeout_test.go`：**新增**，2 test
- `main.go`：版本 `v0.2.146` → `v0.2.147`
- `CLAUDE.md` / `AGENTS.md`：guard 表 101 → 102 项，新增 `NONSTREAM_CALL_TIMEOUT`，
  标记数 240 → 245，保持逐字同步

### 验证

```bash
go build ./...          # 干净
go vet ./internal/server/   # 干净
go test -count=1 ./...   # 全绿

go test ./internal/server/ -run 'NonStreamCallTimeout|NonStreamAsSSE_UsesCallTimeout' -v
    TestNonStreamCallTimeout_Bounded    PASS
    TestNonStreamAsSSE_UsesCallTimeout  PASS
```

`TestNonStreamAsSSE_UsesCallTimeout` 是 AST 检查——测的是「这两行写的是哪个标识符」，
因为唯一会回归的方式就是把 `nonStreamCallTimeout` 改回 `time.Duration(q.timeout)`，
编译期、vet 都零提示。**做了负向验证**：把两处改回 `q.timeout` 后测试如期 FAIL
（两个 handler 都被点出来），改回后恢复。

`TestNonStreamCallTimeout_Bounded` 反射断常量界：
必须 `== upstreamStallTimeout`、`<= 120s`（禁止退化回 300s 量级）、`>= 30s`（禁止误杀长推理），
且类型必须是 `time.Duration`。

### 已知局限

- gateway.go 翻译路径仍无法降级，只能观测。
- 非流式路径仍无 idle 检测，60s 是「总时长」而非「静默时长」判据。上游若在前 55s 内
  开始返回 headers 然后静默，本版本不会主动断——那属于上游行为异常，与本次事故形状不同。
- 上游偶发挂死本身未被解决。本次只把失败从 300s 缩到 60s，让客户端能重新提问。

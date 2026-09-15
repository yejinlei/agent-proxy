# v0.2.146

## 变更：翻译路径大请求跳过原生流式 + 修掉一个潜伏的 stream 标记 bug

两件事。第一件是补齐 `LARGE_BODY_SKIP_STREAM` 在**翻译路径**的缺口；补齐过程中
发现目标函数有一个从没人踩到的 bug——它是**死代码**，一直 0 调用方。

### 1. 翻译路径的大请求 guard

透传路径早就有大请求降级（`LARGE_BODY_SKIP_STREAM`，v0.2.73 血泪教训）：请求体
超过 100KB 就跳过原生流式，走非流式→SSE 包装。原因是感诺对大请求的流式处理会
立即失败，客户端等不及断开。

翻译路径**一直缺这个 guard**。v0.2.144 的日志现场就是它：

```
[route] translation stream=true, calling handleStreamRequest
[upstream] CallStream glm-5.2 → ...
TranslateStream upstream error: status=502 "stream read failed: context canceled"
WS frame → client FAILED: write: broken pipe
```

`agent-proxy-9091.log` 里 6 次 `TranslateStream upstream error`（502 stream read
failed），6 次 broken pipe，全部落在翻译路径。同一份日志里 `took` 最高 41.169s。

阈值抽成包级常量 `largeBodyThreshold`（100KB），透传与翻译共用同一值；判定逻辑
抽成纯函数 `translationStreamRoute` 便于锁阈值边界。

**判据用 `len(downstreamReq)` 而不是 `len(body)`**——翻译后体积会变化，下游体
才是真正发给上游的东西。

### 2. 顺手修掉的潜伏 bug（本版本的真正重点）

降级路由指向 `handleNonStreamResponseAsSSE`。读这个函数时发现：它把
`downstreamReq` **原样**传给 `p.Call`：

```go
resp, headers, err := p.Call(callCtx, downstreamReq, q.info)
```

但 `p.Call` 是**非流式**调用（等完整 JSON 返回），而 `downstreamReq` 带着客户端
的 `"stream":true`。`buildCCRequest` 里 `Stream: req.Stream` 是直传的，
`json:"stream,omitempty"` + `Stream:true` 意味着字段一定写出。

后果链：上游按流式返回 SSE 事件流 → 响应体不是 JSON → `TranslateFromProvider`
反序列化失败。

**为什么一直没炸：这个函数历史上 0 调用方**，是死代码。v0.2.146 的降级路由
引入后它才有调用方，bug 才从潜伏变成可达。对照的 `handlePassthroughNonStreamAsSSE`
一直是正确的（用 `quickRemoveStreamFlag(body)`），这条翻译路径漏了。

修复照抄透传路径的做法，不另起一套：

```go
nsBody := quickRemoveStreamFlag(downstreamReq)
nsBody = q.applyRequestStripper(nsBody)
resp, headers, err := p.Call(callCtx, nsBody, q.info)
```

`applyRequestStripper` 也一起补上——翻译路径的 `downstreamReq` 由 `translateToProvider`
重建，从未经过透传入口的字段过滤。此处是翻译路径唯一真正调用 `p.Call` 的包装点。
对感诺而言是 no-op：`stripSensenovaRequestFields` 剥的是 `guided_grammar` /
`cache_control` / `output_config.effort`，全是 Anthropic/Gemini/Responses 字段，
而 `buildCCRequest` 是强类型 struct，不可能写出这些键。补在这里是为覆盖面，不是为当前生效。

**为什么加专门测试**：这个修复的唯一风险是字符串替换与上游 wire form 差一字节。
`quickRemoveStreamFlag` 用 `strings.Replace` 匹配 `"stream":true`（Go 默认无空格
序列化）和 `"stream": true`（客户端手写/indented JSON）。替换失败是**静默**的——
不报错、不 panic，只是没换掉。所以用 `buildCCRequest` 的**真实产物体**做断言：
先确认它确实产出 `"stream":true`，再确认替换后消失、`"stream":false` 出现、
结果仍是合法 JSON、语义上 `stream` 确实是 `false`。

### gateway.go 的差异（有意分叉）

gateway.go 同名 guard 只打日志、不降级。原因：它没有
`handleNonStreamResponseAsSSE` 包装函数，无法降级。与本文件透传路径
`LARGE_BODY_SKIP_STREAM` 的模式一致。guard 的 `@CONSTRAINT` 里写明这是有意分叉
而非遗漏，要真正降级需先补包装函数，属独立工作量。

### 变更文件

- `internal/server/quick.go`：`largeBodyThreshold` 提为包级常量、新增
  `translationStreamRoute`、翻译路径路由分支加 guard（`LARGE_BODY_SKIP_STREAM_TRANSLATION`）、
  `handleNonStreamResponseAsSSE` 修 stream 标记（`NONSTREAM_AS_SSE_STREAM_FLAG`）
- `internal/server/gateway.go`：`handleStreamRequest` 同名 guard（log-only，双模式同步）
- `internal/server/large_body_route_test.go`：**新增**，7 test
- `internal/server/nonstream_sse_flag_test.go`：**新增**，2 test
- `main.go`：版本 `v0.2.145` → `v0.2.146`
- `CLAUDE.md` / `AGENTS.md`：guard 表 100 → 101 项，新增
  `NONSTREAM_AS_SSE_STREAM_FLAG`，保持逐字同步

### 验证

```bash
go build ./...                                  # 干净
go vet ./internal/server/                       # 干净
go test -count=1 ./...                          # 全绿

go test ./internal/server/ -run 'TestQuickRemoveStreamFlag|TestTranslationStreamRoute|TestLargeBodyThreshold' -v
    TestTranslationStreamRoute/nil 下游体       PASS
    TestTranslationStreamRoute/空 body          PASS
    TestTranslationStreamRoute/小 body          PASS
    TestTranslationStreamRoute/恰好等于阈值      PASS
    TestTranslationStreamRoute/超阈值 1 字节     PASS
    TestTranslationStreamRoute/典型大请求        PASS
    TestLargeBodyThreshold_Value               PASS
    TestQuickRemoveStreamFlag_RealWireForm     PASS   # 用 buildCCRequest 真实产物断言
    TestQuickRemoveStreamFlag_IndentedForm     PASS
```

### 已知局限

- gateway.go 仍无法降级（无包装函数），只能观测。
- `applyRequestStripper` 在翻译路径目前对感诺是 no-op，属防御性覆盖。

# v0.2.141

## 变更：删除 `/v1/models` 重复的响应头转发循环

一次**清理**。修的是响应头重复 —— 每个下游头出现两次。

### 问题

v0.2.140 部署后实测，`GET /v1/models` 的每个响应头都出现两次：

```
Access-Control-Allow-Origin: *      ×2
Access-Control-Expose-Headers: ...  ×2
Date: Mon, 14 Sep 2026 01:35:25 GMT ×2
X-Request-Id: 5fc7c6e9-...          ×2
Content-Length: 527                 （仍正确，不重复）
```

根因是 `handleModels` 里有**两个**头转发循环，都在往 `w.Header().Add()` 追加：

| 位置 | 触发条件 | 过滤 |
|------|---------|------|
| `quick.go:3490` | 每次请求都跑 | 有 |
| `quick.go:3619` | 仅「别名注入」分支 | 有（v0.2.140 补上） |

`Add` 是追加语义，不是覆盖，同一批头加两遍就是两个值。第二个循环遍历的是
`resp.Header` 而不是 `headers`，原 AST 测试只扫 `headers`，所以从没被发现。

### 与 v0.2.140 的关系

这个重复**不是 v0.2.140 引入的**。`git show 65b31c3`（v0.2.139）里两个循环就已经
都在 `Add` 了（`3466` / `3588`），但那时第二个循环**没有过滤**。

所以两个循环各自的职责是清楚的：

- 第一个：正常透传（有过滤），正确。
- 第二个：漏了过滤 —— **这就是 v0.2.140 那次 Content-Length 错位的入口**。

v0.2.140 把第二个循环的过滤补上了（CL 因此修好），但那只是让重复的头不再带 CL。
v0.2.141 直接删掉第二个循环 —— 第一个已经转发过全部头，它没有任何额外作用。

删掉后 `Content-Type` 仍有显式 `Set("application/json")`，行为不变，只是头不再重复。

### 新增 AST 测试锁定

删掉之后没有任何代码「需要」两个循环，但也没有机制阻止后人把它加回来 ——
而「多写一遍转发」正是最容易被当成「顺手补齐」的错误。

新增 `TestNoDuplicateHeaderForwarding`，用 AST 按**函数**分组计数
`range resp.Header` / `range headers` 的 `RangeStmt`，每个函数超过 1 处即失败并
打印函数名和所有行号。

`TestNoUnfilteredHeaderForwarding` 只检查「过滤在不在」，看不出重复 —— 两个
测试各管一件事。

**负向验证**（临时把循环插回去再跑）：

```
--- FAIL: TestNoDuplicateHeaderForwarding/quick.go
    header_forward_test.go:108: quick.go: QuickGateway.handleModels 有 2 处响应头转发循环
    （:3490 :3621）—— w.Header().Add 是追加语义，同一批上游头会被写两次…
```

### 变更文件

- `main.go`：版本 `v0.2.140` → `v0.2.141`
- `internal/server/quick.go`：删除 `handleModels` 第二个转发循环（`-11/+11`，含 guard 注释）
- `internal/server/header_forward_test.go`：**新增** `TestNoDuplicateHeaderForwarding`
  + `guardNoDup`，转发点计数 quick.go 7 → 6（全仓 9 个）
- `CLAUDE.md` / `AGENTS.md`：guard 表 83 → 85 项，转发点计数同步，新增
  `NO_DUPLICATE_HEADER_FWD` 条目，保持逐字同步

不涉及协议翻译器，不触及 quick.go / gateway.go 双模式同步
（`handleModels` 只在 quick.go，`Gateway` 没有头转发）。

### 验证

```bash
go test ./...                        # 全绿
go test ./internal/server/ -run TestNo -v
    TestIsConnectionManagementHeader          PASS
    TestNoUnfilteredHeaderForwarding          PASS  (quick.go 6, gateway.go 3)
    TestNoDuplicateHeaderForwarding           PASS
```

### 已知局限

`/v1/models` 之外的端点没有重复头问题：`/v1/responses`（翻译路径）、`/health`
实测每个头恰好 1 次。

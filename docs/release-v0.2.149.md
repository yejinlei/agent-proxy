# v0.2.149

## 主要修复

### 1. 嵌套 usage 字段：Codex 台账 cache/reasoning 恢复真实值

Codex 的 serde 模型只读 `usage.input_tokens_details.cached_tokens` / `cache_write_tokens`
与 `usage.output_tokens_details.reasoning_tokens`，代理之前只写顶层
`cache_creation_input_tokens` / `cache_read_input_tokens`（还只写到非流式 `TranslateResponse`）。
serde 对未知顶层键是忽略不是报错 → 解析成功 + 字段级丢弃 → 台账恒 0。

修复：出站收敛到单点构造函数 `responsesUsageFromInternal`，三个流式 `lastUsage` 写点
+ 非流式 `TranslateResponse` 全部委托它；入站侧嵌套优先、顶层兜底；details 全零时省略
（Option 语义，空对象等于"明确无缓存"易被误读）。

### 2. 无有效内容超时：Codex 空回合从 5 分钟缩短到 60 秒 + 触发重试

**根因**：上游（感诺 glm-5.2）在模型挂死时会持续发送带 `text:""` 的空 delta 帧维持
连接（日志实测 8925 个空 delta / 5 分钟 = ~3/秒）。`stallTimeoutChan` 只看 line channel
有无活动，每 300ms 都有活动，永不触发。翻译器正确拦下所有空 delta，Codex 只看到
`created + output_item.added + 5s 一次 WS 心跳`。300s 后代理发合法 `response.completed`
（`finish_reason:"stop"`），Codex 判 `ThreadIdleCause::Completed + TurnComplete{error:None}`
→ 静默空回合，`stream_max_retries=5` 全程死代码。

**修复**：`TranslateStream` 内新增 `lastRealContentAt` 追踪，判据是"距离最后一次真实内容
（text delta / reasoning delta / tool_call item）的时间"，不是"空 delta 数量"。60s 阈值
与 `upstreamStallTimeout` 同值同语义，互补覆盖两种"上游挂了"的形态。

命中后发 **`response.failed`** 而非 `response.completed + status:failed`——Codex 只对
`response.failed` 做 `ApiError` 映射（`responses.rs:417`）；`response.completed` 无论
status 都走 `Ok(ResponseEvent::Completed)` → 静默成功。`error.code` 用 `"upstream_timeout"`
避开 Codex 非重试白名单（`context_length_exceeded` / `insufficient_quota` /
`usage_not_included` / `cyber_policy` / `misalignment_policy_violation` /
`invalid_prompt` / `bio_policy` / `server_is_overloaded` / `rate_limit_exceeded`），
映射为 `ApiError::Retryable` → 触发 `stream_max_retries=5` 重试循环，用户看到
`Reconnecting...` 提示。

**防误杀**：三条件联合判断（`nDeltaEvents > 0 && accumulatedText.Len() == 0 &&
reasoningChars == 0 && len(funcCallOrder) == 0`）。`nDeltaEvents > 0` 排除正常网络断连
（ctx.Done / channel close），reasoning delta 会重置计时器避免误杀 o1 / DeepSeek R1 这类
先思考后说话的模型。

## 二进制

- 6 个跨平台二进制（linux/darwin/windows × amd64/arm64）
- `sha256sums.txt` 用可移植裸文件名，`sha256sum -c` 直接可用

## 说明

本次版本号只在二进制里（`main.version=v0.2.149`），代码未提交到 git。

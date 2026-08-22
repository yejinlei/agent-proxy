## v0.2.97

### 修复

**Codex 通过 WebSocket 连接 /v1/responses 返回 405 Method Not Allowed**

- 根因：`/v1/responses` 两端（`quick.go` + `gateway.go`）均使用 `mux.Post(...)` 注册，chi 在方法过滤阶段就将 Codex 的 `GET + Upgrade: websocket` 拦下返回 405，handler 完全未被调用
- `gateway.go` 虽已实现 `handleResponsesWebSocket` 及 WS 检测（`gateway.go:796-797`），但因注册为 `POST` 一直是**死代码**——复杂模式与快速模式都会 405
- 修复：`mux.Post` → `mux.HandleFunc`（不限制方法），handler 按 `Upgrade` 头自行分发 WS / HTTP
- `quick.go`：`handleResponses` 加入 WS 升级检测 + 新增完整 WebSocket 实现（RFC 6455 握手、文本帧编解码、`qwsResponseWriter` 帧化写出），与 `gateway.go` 同步
- Codex 场景：`ws://<proxy>/v1/responses` 握手成功 → 上游 SSE 逐行封装为 WebSocket 文本帧返回，Codex 正常消费

### 影响
- 纯接入层修复，HTTP 调用路径行为不变；新增 WebSocket 接入能力
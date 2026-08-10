## v0.2.32

### 修复
- handlePassthroughNonStreamAsSSE 心跳 goroutine 与响应写入的并发 race：close(done) 从 defer 改为 p.Call() 返回后立即执行，防止 ping 事件与 JSON 响应数据交错
- handlePassthroughNonStreamAsSSE 响应格式错误：Content-Type 为 text/event-stream 但直接写原始 JSON（带换行），现包装为 SSE 格式（event: message + compact JSON + event: done），确保客户端正确解析流式响应

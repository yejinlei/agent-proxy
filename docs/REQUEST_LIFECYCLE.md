# Agent-Proxy — 请求生命周期

> 一个请求经过代理的端到端路径：识别 → 路由 →（透传或翻译）→ 上游 → 响应。
>
> [`ARCHITECTURE.md`](ARCHITECTURE.md) 的配套文档。逐协议字段差异见 [`PROTOCOL_COMPARISON.md`](PROTOCOL_COMPARISON.md)。

---

## 1. 决策树

```mermaid
flowchart TD
    A["入站 POST /v1/{...}"] --> B["识别入站协议"]

    B -->|"POST /v1/chat/completions"| CC[ChatCompletion]
    B -->|"POST /v1/messages"| AN[Anthropic]
    B -->|"POST /v1/responses"| RS[Responses]
    B -->|"POST /v1/models/*:generateContent"| GM[Gemini]

    CC & AN & RS & GM --> C{"入站协议<br/>在上游能力内？"}

    C -->|YES| PASS["<b>透传路径</b><br/>原样转发请求体、<br/>只换模型名"]
    C -->|NO| TRAN["<b>翻译路径</b><br/>走 Central Schema"]

    subgraph T["翻译流水线"]
        TR1["TranslateRequest<br/>入站 → InternalRequest"]
        TR2["TranslateToProvider<br/>InternalRequest → 上游请求体"]
        TR3{Stream?}
        TR3 -->|是| HANdleStream[handleStreamRequest]
        TR3 -->|否| HANdleNon[handleNonStreamResponse]
        TR1 --> TR2 --> TR3
    end

    TRAN --> T

    PASS -->|"stream?"| PS{"stream 字段"}
    PS -->|true| PSS["handlePassthroughStream<br/>上游 SSE 中继"]
    PS -->|false| PNN[handlePassthroughNonStream]

    HANdleStream --> UP["CallStream 调上游"]
    HANdleNon --> UP2["Call 调上游"]
    PSS --> UP
    PNN --> UP2

    UP --> FR["TranslateFromProvider<br/>上游 → InternalResponse"]
    UP2 --> FR

    FR --> OUT["TranslateResponse / TranslateStream<br/>InternalResponse → 入站 SSE"]
    PSS --> OUTP["上游 SSE → 入站包装"]
    PNN --> OUTN["上游响应体 → 入站包装"]

    OUT --> CLIENT[客户端]
    OUTP --> CLIENT
    OUTN --> CLIENT
```

翻译路径的关键顺序：**消息格式转换先于流 / 非流决策**。`InternalRequest.Stream` 在 `TranslateRequest` 阶段捕获并保留，贯穿到最终 handler 分发。

---

## 2. 透传路径（零损耗）

当入站协议匹配上游真实支持的一种时（例如 CC 客户端打到一个 OpenAI 兼容上游），代理跳过所有翻译：

```mermaid
sequenceDiagram
    participant C as 客户端
    participant P as 代理（透传）
    participant U as 上游

    C->>P: POST /v1/chat/completions
    Note over P: 模型别名解析 假→真
    P->>U: POST &lt;base&gt;/&lt;pathPrefix&gt;/chat/completions
    U-->>P: 响应体（JSON 或 SSE）
    Note over P: 模型别名解析 真→假
    P-->>C: 响应体
```

两种变体：

- **`handlePassthroughStream`** —— 中继上游 SSE。如果上游返回的是非流 JSON 而客户端要求 `stream: true`，代理**动态包装**成 SSE。
- **`handlePassthroughNonStream`** —— 转发上游 JSON 响应。如果上游返回的是 SSE 而客户端要求 `stream: false`，代理**动态拆包** SSE、聚合后返回单条 JSON。

正是这种自适应流 / 非流行为，让透传客户端永远察觉不到代理的存在。

---

## 3. 翻译路径（完整 Central Schema 往返）

```mermaid
sequenceDiagram
    participant C as 客户端（如 CC）
    participant P as 代理
    participant T as TranslatorRegistry
    participant U as 上游（如 Anthropic）

    C->>P: POST /v1/chat/completions
    P->>T: TranslateRequest(rawReq)
    Note over T: CC → InternalRequest<br/>（提取 system 到 SystemPrompt、<br/>归一化 tools、捕获 stream 标志）
    T-->>P: InternalRequest

    P->>T: TranslateToProvider(intReq, target="anthropic")
    Note over T: InternalRequest → Anthropic 请求体
    T-->>P: json.RawMessage

    P->>U: POST /v1/messages
    U-->>P: SSE（message_start → … → message_stop）

    loop 对每个上游 SSE 事件
        P->>T: TranslateStreamEvent(rawEvent)
        Note over T: Anthropic → InternalStreamEvent
        T-->>P: InternalStreamEvent
        P->>T: TranslateStream(eventChan, outFn)
        Note over T: InternalStreamEvent → CC SSE chunk
        T-->>P: data: {"choices":[...]}
        P-->>C: data: ...
    end
    P-->>C: data: [DONE]
```

非流变体把上面的 loop 折叠为单次 `Call` → `TranslateFromProvider` → `TranslateResponse` 链。

---

## 4. 各协议 SSE 生命周期

每个协议都有严格的事件序列。翻译器发出完整序列（含取消安全结束），这样严格客户端（Claude Code、Codex）不会拒绝。

### Anthropic

```mermaid
stateDiagram-v2
    [*] --> message_start
    message_start --> content_block_start
    content_block_start --> content_block_delta
    content_block_delta --> content_block_delta
    content_block_delta --> content_block_stop
    content_block_stop --> message_delta
    message_delta --> message_stop
    message_stop --> [*]

    message_start: msg_start<br/>content=[] id=msg_&lt;ts&gt;<br/>output_tokens=0
    content_block_start: citations=[]
    message_delta: 始终发出<br/>（nil 时写默认值）
```

### Responses

```mermaid
stateDiagram-v2
    [*] --> response_created
    response_created --> output_item_added
    output_item_added --> output_text_delta
    output_text_delta --> output_text_delta
    output_text_delta --> output_item_done
    output_item_done --> response_completed
    response_completed --> [*]

    response_created: response 字段（非 data）
    output_text_delta: delta 字段（字符串）
    response_completed: output[] 含完整内容
```

### ChatCompletion

```mermaid
flowchart LR
    A["data: {'choices':[{'delta':{'role':'assistant','content':'hi'}}]}"]
    B["data: {'choices':[{'delta':{'content':'...'}}]}"]
    C["data: {'choices':[{'delta':{},'finish_reason':'stop'}]}"]
    D["data: [DONE]"]
    A --> B --> C --> D
```

### Gemini

```mermaid
flowchart LR
    A["{'candidates':[{'content':{'parts':[{'text':'hi'}]}}]}"]
    B["{'candidates':[{'content':{'parts':[{'text':'...'}]}}]}"]
    A --> B
```

Gemini 的 SSE 是纯 JSON 行（无 `event:` 字段、无 `type` 字段）—— 代理在翻译边界归一化它。

---

## 5. 错误路径

```mermaid
flowchart TD
    ERR["上游错误 / ctx 取消"] --> E{流式?}
    E -->|是| E1["发出 event: error<br/>{'type':'error','error':{...}}"]
    E1 --> E2["如果是 Anthropic，flush content_block_stop / message_delta / message_stop"]
    E2 --> E3["发出 [DONE]"]
    E -->|否| E4["TranslateError(*StreamError)<br/>→ 协议原生错误 JSON"]
    E4 --> E5["返回 5xx + 错误体"]
```

`TranslateError` 是每协议一份（`ErrorTranslator` 接口），让响应形态匹配客户端预期。

---

## 6. Usage 字段兼容

Claude Code 校验 `usage.input_tokens` / `usage.output_tokens`。缺失或 `null` 会 crash 客户端。

- **透传**：`quick.go:fixNullUsageInResponse` 原地修补 null / 缺失 usage：
  - Anthropic 形态（含 `content[]`）→ `{"input_tokens":0,"output_tokens":0}`
  - CC 形态（含 `choices[]`）→ `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`
  - 未知形态 → 两种字段都写，安全兜底
- **翻译**：`InternalResponse.Usage` 必须始终是非空对象（翻译器保证）。
- **CC SSE**：代理注入 `stream_options:{include_usage:true}`，让支持的上游在流里返回 usage 数据。

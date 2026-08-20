# Agent-Proxy — Request Lifecycle

> End-to-end path a request takes through the proxy: detection → routing → (passthrough or translate) → upstream → response.
>
> Companion to [`ARCHITECTURE.md`](ARCHITECTURE.md). For per-protocol field differences see [`PROTOCOL_COMPARISON.md`](PROTOCOL_COMPARISON.md).

---

## 1. The Decision Tree

```mermaid
flowchart TD
    A[Ingress POST /v1/{...}] --> B[Detect ingress protocol]

    B -->|/v1/chat/completions| CC[ChatCompletion]
    B -->|/v1/messages| AN[Anthropic]
    B -->|/v1/responses| RS[Responses]
    B -->|/v1/models/*:generateContent| GM[Gemini]

    CC & AN & RS & GM --> C{Ingress protocol<br/>in upstream capabilities?}

    C -->|YES| PASS[<b>Passthrough path</b><br/>forward body verbatim,<br/>only swap model name]
    C -->|NO| TRAN[<b>Translate path</b><br/>through Central Schema]

    subgraph T["Translate pipeline"]
        TR1[TranslateRequest<br/>ingress → InternalRequest]
        TR2[TranslateToProvider<br/>InternalRequest → provider body]
        TR3{Stream?}
        TR3 -->|yes| HANdleStream[handleStreamRequest]
        TR3 -->|no| HANdleNon[handleNonStreamResponse]
        TR1 --> TR2 --> TR3
    end

    TRAN --> T

    PASS -->|stream?| PS{stream field}
    PS -->|true| PSS[handlePassthroughStream<br/>upstream SSE relay]
    PS -->|false| PNN[handlePassthroughNonStream]

    HANdleStream --> UP[CallStream to upstream]
    HANdleNon --> UP2[Call to upstream]
    PSS --> UP
    PNN --> UP2

    UP --> FR[TranslateFromProvider<br/>upstream → InternalResponse]
    UP2 --> FR

    FR --> OUT[TranslateResponse / TranslateStream<br/>InternalResponse → ingress SSE]
    PSS --> OUTP[upstream SSE → ingress wrapper]
    PNN --> OUTN[upstream body → ingress wrapper]

    OUT --> CLIENT[Client]
    OUTP --> CLIENT
    OUTN --> CLIENT
```

The critical order in the translate path: **message-format conversion happens before the stream/non-stream decision**. `InternalRequest.Stream` is captured during `TranslateRequest` and preserved through to the final handler dispatch.

---

## 2. Passthrough Path (zero-loss)

When the ingress protocol matches one the upstream actually speaks (e.g., a CC client hitting an OpenAI-compatible upstream), the proxy skips all translation:

```mermaid
sequenceDiagram
    participant C as Client
    participant P as Proxy (passthrough)
    participant U as Upstream

    C->>P: POST /v1/chat/completions
    Note over P: model alias resolve fake→real
    P->>U: POST <base>/<pathPrefix>/chat/completions
    U-->>P: body (JSON or SSE)
    Note over P: model alias resolve real→fake
    P-->>C: body
```

Two variants:

- **`handlePassthroughStream`** — relays upstream SSE. If the upstream returned non-stream JSON while the client asked for `stream: true`, the proxy **wraps** the one-shot response into SSE on the fly.
- **`handlePassthroughNonStream`** — forwards the upstream JSON response. If the upstream returned SSE while the client asked for `stream: false`, the proxy **unwraps** the SSE, aggregates, and returns a single JSON response.

This adaptive stream-vs-non-stream behavior is why passthrough clients never notice the proxy.

---

## 3. Translate Path (full Central Schema round-trip)

```mermaid
sequenceDiagram
    participant C as Client (e.g. CC)
    participant P as Proxy
    participant T as TranslatorRegistry
    participant U as Upstream (e.g. Anthropic)

    C->>P: POST /v1/chat/completions
    P->>T: TranslateRequest(rawReq)
    Note over T: CC → InternalRequest<br/>(extract system to SystemPrompt,<br/>normalize tools, capture stream flag)
    T-->>P: InternalRequest

    P->>T: TranslateToProvider(intReq, target="anthropic")
    Note over T: InternalRequest → Anthropic body
    T-->>P: json.RawMessage

    P->>U: POST /v1/messages
    U-->>P: SSE (message_start → ... → message_stop)

    loop for each upstream SSE event
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

The non-stream variant collapses the loop into a single `Call` → `TranslateFromProvider` → `TranslateResponse` chain.

---

## 4. SSE Lifecycle per Protocol

Each protocol has a strict event sequence. The translators emit the full sequence (including cancellation-safe teardown) so strict clients (Claude Code, Codex) don't reject the stream.

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

    message_start: msg_start<br/>content=[] id=msg_<ts><br/>output_tokens=0
    content_block_start: citations=[]
    message_delta: always emitted<br/>(default usage if nil)
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

    response_created: response field (not data)
    output_text_delta: delta field (string)
    response_completed: output[] with full content
```

### ChatCompletion

```mermaid
flowchart LR
    A[data: {"choices":[{"delta":{"role":"assistant","content":"hi"}}]}]
    B[data: {"choices":[{"delta":{"content":"..."}}]}]
    C[data: {"choices":[{"delta":{},"finish_reason":"stop"}]}]
    D[data: [DONE]]
    A --> B --> C --> D
```

### Gemini

```mermaid
flowchart LR
    A[{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}]
    B[{"candidates":[{"content":{"parts":[{"text":"..."}]}}]}]
    A --> B
```

Gemini's SSE is plain JSON lines (no `event:` field, no `type` field) — the proxy normalizes this on the translate boundary.

---

## 5. Error Path

```mermaid
flowchart TD
    ERR[Upstream error / ctx cancellation] --> E{streaming?}
    E -->|yes| E1[emit event: error<br/>{"type":"error","error":{...}}]
    E1 --> E2[flush content_block_stop / message_delta / message_stop if Anthropic]
    E2 --> E3[emit [DONE]]
    E -->|no| E4[TranslateError(*StreamError)<br/>→ protocol-native error JSON]
    E4 --> E5[return 5xx with error body]
```

`TranslateError` is per-protocol (`ErrorTranslator` interface) so the response shape matches what the client expects.

---

## 6. Usage-Field Compatibility

Claude Code validates `usage.input_tokens` / `usage.output_tokens`. Missing or `null` usage crashes the client.

- **Passthrough**: `quick.go:fixNullUsageInResponse` patches null/missing usage inline:
  - Anthropic-shaped (has `content[]`): `{"input_tokens":0,"output_tokens":0}`
  - CC-shaped (has `choices[]`): `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`
  - Unknown shape: writes both sets of fields as a safe fallback.
- **Translate**: `InternalResponse.Usage` must always be a non-null object (translators guarantee this).
- **CC SSE**: the proxy injects `stream_options:{include_usage:true}` so upstreams that support it return usage data in the stream.
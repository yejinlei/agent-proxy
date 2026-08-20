# Agent-Proxy — Protocol Comparison

> Field-by-field differences between the four protocols the proxy translates between. This is the reference the translators use to normalize into / out of the Central Schema.
>
> Companion to [`ARCHITECTURE.md`](ARCHITECTURE.md) (Central Schema) and [`REQUEST_LIFECYCLE.md`](REQUEST_LIFECYCLE.md) (event sequences).

---

## 1. Endpoint & System Prompt

```mermaid
flowchart TB
    subgraph CC[ChatCompletion]
        CC_E[POST /v1/chat/completions]
        CC_S[messages[0].role == "system"]
    end
    subgraph AN[Anthropic]
        AN_E[POST /v1/messages]
        AN_S[top-level system<br/>(string | array)]
    end
    subgraph GM[Gemini]
        GM_E[POST /v1/models/{model}:generateContent]
        GM_S[top-level systemInstruction]
    end
    subgraph RS[Responses]
        RS_E[POST /v1/responses]
        RS_S[top-level instructions]
    end

    CC_S & AN_S & GM_S & RS_S --> NORM[InternalRequest.SystemPrompt]
```

| | ChatCompletion | Anthropic | Gemini | Responses |
|---|---|---|---|---|
| Endpoint | `POST /v1/chat/completions` | `POST /v1/messages` | `POST /v1/models/{model}:generateContent` | `POST /v1/responses` |
| System prompt | `messages[0].role=="system"` | top-level `system` (str or array) | top-level `systemInstruction` | top-level `instructions` |
| Role set | user / assistant / system / tool | user / assistant (system extracted) | user / model (system extracted) | user / assistant (system extracted) |

**Central Schema rule**: system is always extracted to `InternalRequest.SystemPrompt`; `InternalRequest.Messages` never contains system. When translating back to CC, the system message is reinserted as `messages[0]`.

---

## 2. Content (Text + Multimodal)

```mermaid
flowchart TB
    CC_C[CC: content = string | ContentBlock[]<br/>{type:"image_url", image_url:{url:"data:..."}}]
    AN_C[Anthropic: content = ContentBlock[]<br/>{type:"image", source:{type:"base64", data, media_type}}]
    GM_C[Gemini: parts = Part[]<br/>{inline_data:{data, mime_type}}]
    RS_C[Responses: content = ContentBlock[]<br/>{type:"image", source:{type:"base64", data, media_type}}]

    CC_C & AN_C & GM_C & RS_C --> N[InternalContentBlock<br/>Type, Text, Data, MediaType, URL,<br/>FileName, FileURI, Thinking, Signature, _raw]
```

| | Text | Image (base64) | Image (URL) | File | Thinking |
|---|---|---|---|---|---|
| ChatCompletion | `content: string` or `{type:"text",text}` | `{type:"image_url", image_url:{url:"data:image/..."}}` | `{type:"image_url", image_url:{url:"https://..."}}` | — | — |
| Anthropic | `{type:"text",text}` | `{type:"image", source:{type:"base64",data,media_type}}` | `{type:"image", source:{type:"url",url}}` | — | `{type:"thinking",thinking}` |
| Gemini | `{text:"..."}` inside `parts` | `{inline_data:{data,mime_type}}` inside `parts` | `{file_data:{file_uri,mime_type}}` | `{file_data:{...}}` | — |
| Responses | `{type:"output_text",text}` | `{type:"image", source:{type:"base64",data,media_type}}` | `{type:"image", source:{type:"url",url}}` | — | — |

`InternalContentBlock` unifies these with `_raw` (`json.RawMessage`) as a safety net for protocol-specific extras.

---

## 3. Tools / Function Calling

The trickiest compatibility surface.

**Definitions (what the client sends):**

```mermaid
flowchart TB
    CC_T[CC: tools[{type:"function", function:{name, description, parameters}}]]
    AN_T[Anthropic: tools[{name, description, input_schema}]]
    GM_T[Gemini: tools[{functionDeclarations:[{name, description, parameters}]}]]
    RS_T[Responses: tools[{type:"function", name, parameters}]<br/>no description field]

    CC_T & AN_T & GM_T & RS_T --> N[InternalTool<br/>Type, Function{Name, Description, Parameters}, InputSchema]
```

**Calls (what the model returns):**

| Protocol | Where | ID naming | Arguments form |
|---|---|---|---|
| ChatCompletion | `message.tool_calls: [{id, type:"function", function:{name, arguments}}]` | arbitrary | `arguments` = JSON **string** |
| Anthropic | inside `content: [{type:"tool_use", id, name, input}]` | `toolu_xxx` | `input` = JSON **object** |
| Gemini | `parts: [{functionCall:{name, args}}]` | none (proxy generates ID mapping) | `args` = JSON **object** |
| Responses | `tool_calls: [{id, type:"function", name, input}]` | `responses_xxx` | `input` = JSON **object** |

**Results (what gets fed back):**

| Protocol | Shape |
|---|---|
| ChatCompletion | `messages[{role:"tool", tool_call_id, content:"result"}]` |
| Anthropic | `messages[{role:"user", content:[{type:"tool_result", tool_use_id, content}]}]` |
| Gemini | `contents[{role:"user", parts:[{functionResponse:{name, response:{result}}}]}]` |
| Responses | `input[{type:"message", role:"user", content:[{type:"tool_result", ...}]}]` |

**Key compatibility rules:**

1. `arguments` (CC) is a JSON **string** ↔ `input`/`args` (others) are JSON **objects** → translators Marshal/Unmarshal at the boundary.
2. Anthropic hides `tool_calls` inside `content[]` — parsing must scan every content block.
3. Gemini has no `tool_call_id` concept → the proxy generates unique IDs and maintains a mapping.
4. `_raw` on `InternalContentBlock` preserves protocol-specific tool fields.

---

## 4. Response Format (structured output)

| | Text | JSON object | JSON schema |
|---|---|---|---|
| ChatCompletion | `{type:"text"}` | `{type:"json_object"}` | — |
| Anthropic | — | — | tool-based or new `response_format` |
| Gemini | `responseMimeType:"text/plain"` | `responseMimeType:"application/json"` | via `schema` |
| Responses | `{type:"text"}` | `{type:"json_object"}` | `{type:"json_schema", json_schema:{...}}` |

Unified as `InternalResponseFormat{Type, JsonSchema}`.

---

## 5. Usage (token accounting)

Claude Code clients crash if `usage` is null/missing.

| | Input | Output | Total | Cache-specific |
|---|---|---|---|---|
| ChatCompletion | `prompt_tokens` | `completion_tokens` | `total_tokens` | — |
| Anthropic | `input_tokens` | `output_tokens` | `total_tokens` | — |
| Gemini | `prompt_token_count` | `candidates_token_count` | `total_token_count` | — |
| Responses | `input_tokens` | `output_tokens` | `total_tokens` | `cache_creation_tokens`, `cache_read_tokens` |

Unified as `InternalUsage{PromptTokens, CompletionTokens, TotalTokens, CacheCreationTokens, CacheReadTokens}`.

The proxy also back-fills missing usage in passthrough responses (`quick.go:fixNullUsageInResponse`) with sensible zeros:
- Anthropic-shaped (`content[]`) → `{"input_tokens":0,"output_tokens":0}`
- CC-shaped (`choices[]`) → `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`
- Unknown → both sets, as a safe fallback.

---

## 6. SSE Event Format

| | Line format | Start | Delta | End |
|---|---|---|---|---|
| ChatCompletion | plain `data:` | first chunk carries `role:"assistant"` | `delta:{content:"..."}` | `finish_reason:"stop"` then `data: [DONE]` |
| Anthropic | `{type:"..."}` inside `data:` | `message_start` (content=[], id=msg_<ts>, output_tokens=0) | `content_block_delta:{delta:{type:"text_delta",text}}` | `content_block_stop → message_delta → message_stop` |
| Gemini | raw JSON per line, no `event:` | `candidates[0].content.parts[0].text` | same shape, accumulating | last chunk carries `usageMetadata` |
| Responses | `event: response.*` named | `response.created` (data in `response` field) | `response.output_text.delta` (data in `delta` field) | `response.output_item.done → response.completed` (data in `response` field) |

The proxy's translators enforce the **full** event lifecycle for each protocol, including cancellation-safe teardown (emitting the closing events even if the upstream channel closes early). This is what keeps strict clients (Claude Code, Codex) from rejecting the stream.

---

## 7. Quick Reference: Which Translator Handles What

```mermaid
graph LR
    subgraph IN[Ingress]
        CC_T[chatcompletion/translator.go]
        AN_T[anthropic/translator.go]
        GM_T[gemini/translator.go]
        RS_T[responses/translator.go]
    end
    SC[schema/internal.go]
    subgraph OUT[Provider]
        CC_P[provider/openai.go<br/>NewOpenAIClient(+WithPath)]
        AN_P[provider/anthropic.go]
        GM_P[provider/gemini.go]
        RS_P[provider/responses.go]
    end

    CC_T <--> SC <--> CC_P
    AN_T <--> SC <--> AN_P
    GM_T <--> SC <--> GM_P
    RS_T <--> SC <--> RS_P
```
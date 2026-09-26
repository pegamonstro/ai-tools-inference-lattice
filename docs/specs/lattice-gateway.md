# Spec: Lattice Gateway (Phase 3)

**Status:** Draft
**Host:** Mac (Apple M1 16GB)

The Lattice Gateway is the execution arm for all local inference. It translates `inference.v1` requests into Ollama calls and manages the local hardware resources to protect the system's health.

## 1. Core Responsibilities

1. **Protocol Implementation**: Serve the `inference.v1` API.
2. **Ollama Adaptation**: Translate `inference.v1` requests into Ollama's `/api/chat` or `/v1/chat/completions` calls.
3. **Memory-Aware Concurrency (Swap Avoidance)**: prevent memory over-subscription to protect the Mac's SSD from swap thrashing.
4. **Telemetry**: Track actual local latency, token throughput, and memory pressure.

## 2. Swap Avoidance Strategy

The Mac has 16GB of unified memory. Running multiple LLMs or very large contexts can trigger swap.

**The "No-Swap" Guard**:
- **Resident Model Set**: The Gateway should ideally pin a set of common models (e.g., `granite4:3b` as main) to avoid constant unloading/reloading (the `OLLAMA_MAX_LOADED_MODELS=1` issue).
- **Concurrency Limit**: Maintain a hard limit on concurrent local requests based on the active model's memory footprint.
- **Pressure Signal**: If the system reports high memory pressure (or if the calculated memory usage of active requests exceeds a threshold), the Gateway must:
  - Queue new requests.
  - Or return `429 Too Many Requests` to signal the Control plane to reroute to cloud.

## 3. Implementation Logic

When a request arrives from the Control plane:

1. **Identify Model**: Get the mapped resident model name.
2. **Check Memory Budget**:
   - Calculate: `(Number of active requests * Avg context size) + Model size`
   - If `Estimated Memory > Budget` $\rightarrow$ Queue/Reject.
3. **Execute**: Call Ollama.
4. **Telemetry**: Record wall-clock time, token count, and peak memory usage.
5. **Return**: Standard OpenAI response.

### 3.1 Tool calling

The gateway forwards `tools` and `tool_choice` on both provider paths and
translates Ollama's reply back into OpenAI's shape:

- **Forwarding.** `tools` is passed on the `/api/chat` call; when the client
  sends none the key is omitted entirely (Ollama rejects `tools: null`).
  `tool_choice` is forwarded when the client supplies it.
- **Arguments are re-typed.** Ollama returns
  `tool_calls[].function.arguments` as a **JSON object**; OpenAI defines the same
  field as a **string**. The gateway decodes the arguments as raw JSON and
  re-encodes them as a string, so the client receives the OpenAI shape.
- **Call ids are synthesized.** Ollama issues no call id, and OpenAI clients key
  the tool result they send back on one, so the gateway assigns `call_%d` by
  position.
- **`finish_reason` becomes `tool_calls`.** When the reply carries tool calls,
  the choice's `finish_reason` is `"tool_calls"`; otherwise `"stop"`. An agent's
  loop branches on this — `"stop"` with tool calls present makes it end its turn
  instead of calling the tool.

> **Known limitation — streaming tool calls are unary-only.** A streamed answer
> is **not** translated to `tool_calls`. The SSE path emits content deltas and
> terminates with `finish_reason: "stop"`, carrying no tool calls. Tool calling
> is available on the unary path only, so a tool-calling client must not set
> `stream: true`. This is a deliberate non-goal, not a defect.

### 3.2 Health and the context ceiling

`GET /health` returns JSON:

```json
{ "status": "ok", "max_context": 65536 }
```

`max_context` is the ceiling the gateway will honour
(`LATTICE_GATEWAY_MAX_CONTEXT`, default `65536`). It is reported rather than
configured twice: the gateway is the only process that knows what a context
window costs in KV cache on this hardware, so it is the authority on the number
and the Control plane relays it — from here into `GET /capabilities`
([`lattice-control.md`](lattice-control.md) §3.3). A prompt that would need more
than this ceiling is sized down to it.

## 4. Implementation Plan

- **Language**: Go.
- ** API**: HTTP server on `:8081`.
- **Exit Test**:
  - Send a request from the Control plane $\rightarrow$ Gateway executes on Mac $\rightarrow$ return result.
  - Simulate memory pressure $\rightarrow$ Gateway rejects/queues request.
  - Measure and record actual resident latency (warm vs cold).

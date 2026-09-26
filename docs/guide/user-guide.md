# Lattice User Guide

How to send work to Lattice and get inference back. This guide is written for
someone with a client — a script, an editor plugin, a chat UI — who wants to use
the lattice without caring where the work actually runs.

If you are installing or operating Lattice rather than using it, read the
[Operations Manual](../manual/operations-manual.md) instead.

---

## 1. The one thing to know

Lattice speaks the **OpenAI Chat Completions API**. You point an ordinary
OpenAI-compatible client at the frontend and it works — no SDK fork, no custom
transport.

```
base URL   http://<frontend-host>:8080/v1
path       /v1/chat/completions
API key    anything (ignored)
model      a capability alias — or any literal model name, passed through
```

There is **one** entry point (the frontend). You never talk to the control
plane or the gateway directly in normal use.

You can check what is available before you call: `GET /v1/models` lists the
aliases and the context ceiling, and `GET /health` reports liveness.

---

## 2. Model names: alias or literal

There are two ways to fill the `model` field.

**A capability alias.** Lattice routes on *intent*, not on a model name. The
alias is resolved by the control plane to a real model per target:

| alias | local model | cloud model |
|---|---|---|
| `local-brain` | `granite4:3b` | `gemma4:31b-cloud` |
| `local-coder` | `hermes3:8b` | `deepseek-v4-pro:cloud` |

Ask for `local-coder` and you get the 8B local model on the Mac — or, if the
request is interactive and cloud is permitted, the cloud model instead. The
choice is made for you.

**A literal model name.** Any string that is not one of the aliases above is
passed through **verbatim** as the model to run. `model: "hermes3:8b"` runs that
exact model on the local gateway. Use this when your client is configured with a
real Ollama model id rather than a Lattice alias.

> **A literal name is never silently dropped.** If you name a model, that name
> reaches the target: an unknown one fails with the target's own error (a `500`
> that names the model), never an empty model field. The name you sent is the
> name telemetry shows.

To see the aliases and the context ceiling, call `GET /v1/models`.

---

## 3. Routing: the `routing` envelope

The `routing` object is optional. It is the only extension beyond OpenAI, and
it is how you express *where* the work is allowed to go.

```json
{
  "model": "local-coder",
  "messages": [{ "role": "user", "content": "Reverse a string in Python." }],
  "routing": {
    "privacy": "LOCAL_ONLY",
    "latency_class": "batch",
    "parallelism": 1,
    "request_id": "my-run-001",
    "provider_params": {
      "reasoning_effort": "medium",
      "max_budget": 4096
    }
  }
}
```

| field | values | meaning |
|---|---|---|
| `privacy` | `LOCAL_ONLY` \| `LOCAL_PREFERRED` \| `CLOUD_ALLOWED` | `LOCAL_ONLY` is a hard gate: if no local gateway is available the request **fails with 503**. It will never silently reach the cloud. |
| `latency_class` | `interactive` \| `batch` | `interactive` prefers the cloud (fast, budgeted); `batch` prefers the local gateway (free, slow). |
| `parallelism` | int | advisory concurrency hint. |
| `request_id` | string | your correlation id. It is echoed through all three telemetry streams — use it to trace a request. |
| `provider_params.reasoning_effort` | `low` \| `medium` \| `high` | raises the output ceiling and the context window for reasoning models. |
| `provider_params.max_budget` | int | caps output tokens (default 4096). |

**Omit `routing` entirely** and you get the safe default: the local path.

### Decision table

| privacy | latency_class | target |
|---|---|---|
| `LOCAL_ONLY` | any | local gateway, or **503** if unhealthy — never cloud |
| anything else | `interactive` | cloud |
| anything else | `batch` | local gateway |

---

## 4. Streaming

Streaming is supported and works the way you expect:

```
POST /v1/chat/completions   { ..., "stream": true }
→ Content-Type: text/event-stream
  data: {"object":"chat.completion.chunk","choices":[{"delta":{"content":"..."},...}]}
  ...
  data: {"choices":[{"delta":{},"finish_reason":"stop",...}]}
  data: [DONE]
```

Any OpenAI-compatible streaming client works unmodified. Non-streaming requests
return a single `application/json` completion.

---

## 5. Examples

### curl

```bash
curl -X POST http://<frontend-host>:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "local-brain",
    "messages": [{"role": "user", "content": "In one sentence, what is an inference lattice?"}]
  }'
```

### Python — official OpenAI SDK

```python
from openai import OpenAI

client = OpenAI(base_url="http://<frontend-host>:8080/v1", api_key="unused")

# Plain call. The model field selects a capability.
r = client.chat.completions.create(
    model="local-brain",
    messages=[{"role": "user", "content": "Name three colours."}],
)
print(r.choices[0].message.content, r.usage)

# Streamed call.
for chunk in client.chat.completions.create(
    model="local-brain",
    messages=[{"role": "user", "content": "Count to five."}],
    stream=True,
):
    print(chunk.choices[0].delta.content or "", end="")
```

**Passing the routing envelope from a stock SDK.** The SDK forwards unknown
top-level fields verbatim via `extra_body`, so you can apply routing policy
without modifying the request schema:

```python
r = client.chat.completions.create(
    model="local-coder",
    messages=[{"role": "user", "content": "Write a parser."}],
    extra_body={"routing": {
        "request_id": "run-42",
        "privacy": "LOCAL_ONLY",
        "latency_class": "batch",
    }},
)
```

### `lattice-cli`

The bundled CLI is a thin wrapper for quick local checks:

```
$ lattice-cli "Summarise the last paragraph."
```

---

## 6. What to expect from each path

| | local (Mac gateway) | cloud |
|---|---|---|
| cost | free | burns subscription budget |
| speed | ~10 tok/s warm; slower cold or with long context | fast |
| concurrency | serialised locally (1 at a time) | up to 3 in parallel |
| privacy | never leaves the network | leaves the network |

The local path is deliberately serialised so the Mac never thrashes its SSD.
Do not expect parallel throughput from it; do expect it to always work when the
gateway is healthy.

---

## 7. Errors

| status | body | meaning |
|---|---|---|
| `503` | `Control plane: No healthy local gateway found` | A `LOCAL_ONLY` (or default) request had no healthy local gateway. **This is the sovereignty guarantee working** — it did not fall back to cloud. Retry when the gateway recovers. |
| `503` | `Control plane unavailable or timed out` | The frontend could not reach the control plane. |
| `429` | `Local memory pressure: available RAM below safety margin` | The gateway refused the request to avoid forcing the Mac into swap. Retry later. |
| `500` | `ollama returned status N` | The local provider failed (e.g. model not found, provider down). |
| `400` | unmarshal error | Malformed JSON body. |

Every failure is also written to the telemetry stream with an `error` field, so
a failed request is visible on the operations display rather than disappearing.

---

## 8. Tracing a request

Every response from Lattice carries an **`X-Request-Id`** header. If you set
`routing.request_id`, that id is echoed back; if you send none, Lattice generates
one and returns it there — so an id is always available even when your client has
never heard of the `routing` envelope. Read the header if you did not supply an
id and want to find the request in the logs.

That id is attached to the control-plane decision, the frontend completion, and
the gateway execution, so you can follow one request across all three planes in
the telemetry logs. See
[Observability](../handbook/architecture-handbook.md#telemetry-pipeline) in the
handbook.

> **Tool calls are unary-only.** Tool calling works on the unary path; a streamed
> request is not translated to `tool_calls`. If your client uses tools, send
> `stream: false`.

---

Authored by [pegamonstro](https://github.com/pegamonstro) — The Bikini Club.
Licensed under the [Apache License 2.0](../../LICENSE).

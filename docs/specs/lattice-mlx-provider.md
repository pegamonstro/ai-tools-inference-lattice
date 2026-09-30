# Spec: MLX Provider — a second local runtime behind the Gateway

**Status:** In progress
**Host:** Mac (Lattice Gateway)
**Gate:** none — the code lands now; the second runtime is enabled by config, not by this change

This spec records how the Gateway grows a second local provider, **MLX**,
alongside Ollama. It is the concrete follow-through on the P1 measurement in
[`docs/measurements.md`](../measurements.md) (MLX decodes ~43 % faster than
llama.cpp on the M1) and the scoping in
[`docs/specs/lattice-apple-acceleration.md`](lattice-apple-acceleration.md).
The Gateway stops hardcoding `providers["ollama"]` and becomes a real
provider registry, driven by a JSON file, that announces what it serves to
the control plane.

## 1. Why a second provider

The Mac is the only host allowed to compute local models. Today every local
request funnels through Ollama (llama.cpp). MLX is Apple's own framework and
decodes faster on the same hardware — a runtime win, not a model win. Two
providers means each local model can run on the engine that suits it:

- **Ollama** stays the default: it is the incumbent, holds the tool-calling
  translation, the embeddings path, and every model already installed.
- **MLX** is the opt-in fast path for specific models, reached by config.

The concurrency model is unchanged: Lattice serialises *local* inference
(one local model resident at a time), whether that model runs on Ollama or
MLX. Cloud models run concurrently with local on the cloud provider, as
before — this spec touches only the local side.

## 2. The registry replaces a hardcode

The Gateway's `providers` map is today populated with exactly one entry,
`providers["ollama"]`, and `handleInference` looks up that literal key. This
spec removes both: the map is built from a config file, and the lookup
resolves a *model* to a *provider* through that same config.

### 2.1 Config file — `LATTICE_GATEWAY_PROVIDERS`

An environment variable names a JSON file; the Gateway reads it at startup.
When the variable is unset, the Gateway runs with the current single-provider
behaviour (Ollama only, every model passed through) — so an existing
deployment is unaffected until it opts in.

```json
{
  "default_provider": "ollama",
  "providers": [
    { "name": "ollama", "kind": "ollama", "endpoint": "http://localhost:11434" },
    { "name": "mlx",    "kind": "mlx",    "endpoint": "http://localhost:8080" }
  ],
  "models": [
    {
      "name": "qwen2.5-coder:3b",
      "provider": "mlx",
      "upstream": "mlx-community/Qwen2.5-Coder-3B-Instruct-4bit"
    }
  ]
}
```

| field | meaning |
|---|---|
| `default_provider` | provider for any model not listed in `models` (passthrough name) |
| `providers[].name` | registry key; also the name `Name()` reports |
| `providers[].kind` | `"ollama"` or `"mlx"` — selects the concrete implementation |
| `providers[].endpoint` | base URL the provider POSTs to |
| `models[].name` | the local model id a client names |
| `models[].provider` | which provider serves it |
| `models[].upstream` | the name to send upstream (the MLX HF repo id); defaults to `name` |

The `kind` field is the one switch the code keys on — it is data choosing
between two compiled-in implementations, not a registry of pluggable
binaries. `models` maps a local alias to an upstream id because MLX names a
model by its Hugging Face repo, whereas Ollama names it by tag
(`qwen2.5-coder:3b`). The mapping is config, not code, so a model moves
between engines without a rebuild.

### 2.2 Resolution and the fail-loudly rule

```go
func resolveModel(model string) (provider, upstream string) {
    if p, ok := modelProviders[model]; ok {
        u := model
        if v, ok := modelUpstream[model]; ok {
            u = v
        }
        return p, u
    }
    return defaultProvider, model
}
```

`handleInference` resolves the name, looks the provider up in the registry,
and — on a miss — **fails loudly**: `500 No provider registered for model`,
never a silent fall to Ollama. The same applies at request time: if the
resolved provider is down (connection error or a non-200), the request
fails with that error. There is no promotion, no demotion, no retry on the
other engine. A model mapped to MLX is served by MLX or not at all. This is
the same sovereignty posture as `LOCAL_ONLY` in the control plane: a
failure is visible and attributed, never laundered into a different target.

The name the client sent is the name telemetry shows (unchanged): resolution
rewrites only the upstream name handed to the provider, never the telemetry
field, so an MLX-served request still logs `qwen2.5-coder:3b`, not the HF id.

## 3. MLXProvider — chat-only, OpenAI-compatible client

MLX is a Python/Metal library, not a bundled server, so the provider is an
HTTP client pointing at an `mlx-lm` server process (started with
`mlx_lm.server`, which exposes an OpenAI-compatible
`/v1/chat/completions`). The provider implements the existing `Provider` and
`StreamingProvider` interfaces; `kind: "mlx"` selects it.

### 3.1 Unary (`Execute`)

POSTs to `<endpoint>/v1/chat/completions` with `model` = the resolved
upstream id, `messages`, `stream: false`, and `max_tokens` from the existing
`resolveMaxTokens` budget. Because mlx-lm already speaks OpenAI's shape, the
response decodes directly into the Gateway's own `Response`/`Choice`/`Usage`
types — there is no translation step the way Ollama's native `/api/chat`
needs one.

### 3.2 Streaming (`ExecuteStream`)

POSTs the same body with `stream: true` plus
`stream_options: {"include_usage": true}` so the terminal chunk carries token
counts. The provider scans the upstream SSE lines, accumulates `delta.content`,
re-emits through the Gateway's `writeSSEChunk`, and assembles the `Response`
for telemetry — the same shape as `OllamaProvider.ExecuteStream`, so the
streaming client contract (finish_reason, `[DONE]`) is identical.

### 3.3 Chat-only, for now

The MLX provider forwards `messages` and returns text. It does **not**
translate tool calls, does **not** set `num_ctx`/KV-cache options (mlx-lm
sets its own context at server load), and does **not** serve embeddings.
The Ollama provider remains the tool-calling and embeddings path. If a
request carrying `tools` is resolved to MLX, it is forwarded as-is and the
model's own answer is returned — the refusal to special-case it is
deliberate scope, not a gap, and it is revisited only if a tool-calling
model is ever routed to MLX.

### 3.4 Shared transport

Like `OllamaProvider`, `MLXProvider` builds one `http.Transport` on first
use (`sync.Once`) and clones `http.DefaultTransport`, so streamed answers
reuse connections instead of stranding one per request.

## 4. Capability announcement

The Gateway's `/health` today returns `{"status","max_context"}`. It grows a
`providers` field so the control plane — which polls `/health` every 10 s —
learns what the Gateway actually serves, instead of assuming "everything via
Ollama":

```json
{
  "status": "ok",
  "max_context": 32768,
  "providers": {
    "ollama": ["granite3-moe:3b", "granite4:3b"],
    "mlx":    ["qwen2.5-coder:3b"]
  }
}
```

The per-provider list is the set of *local model names explicitly routed to
that provider* in the config. The default provider's list is left to mean
"everything else not named" — it is not an exhaustive model inventory, which
would duplicate Ollama's `/api/tags`. The control plane stores the
announcement on each poll (alongside `max_context`) so future routing can
prefer a gateway that actually hosts a requested model; the announcement is
read-only discovery and changes no routing decision in this change.

Health itself is unchanged in meaning: the Gateway is healthy while memory
is within margin and its **default** provider is reachable. A mapped
provider that is down does not take the whole Gateway down — its models fail
loudly at request time, and `providers` still lists them so the control plane
can see the intended shape.

## 5. Not in scope

- **Tool calling on MLX** — chat-only first (see 3.3).
- **Embeddings on MLX** — Ollama keeps `/v1/embeddings`.
- **Concurrent local inference** — the single local slot still serialises
  both providers; one resident model at a time.
- **The M6 / 32 GB** — this change is config-only; enabling a second
  resident runtime on 16 GB is the operator's decision, and the M6 makes it
  comfortable (see `lattice-apple-acceleration.md` §3).
- **A running `mlx-lm` server** — the provider is a client; standing up the
  Python server is deployment, not this code.

## 6. Decision log

- **2026-09-29:** JSON file, chat-only first, fail loudly — confirmed with
  the operator before this spec. Registry is data-driven, `kind` is the only
  code-side branch, and a mapped provider that is down fails the request
  rather than falling back to Ollama.

# Spec: Lattice Control (Phase 2)

**Status:** Draft
**Host:** RPi4

The Lattice Control plane is the intelligence of the system. It does not execute inference; it performs the **routing decision**.

## 1. Core Responsibilities

1. **Routing Decision**: Map an `inference.v1` request to a specific execution target (Mac Gateway or Cloud Provider).
2. **Capability Registry**: Track which models are resident on the Mac and which are available in the cloud.
3. **Concurrency Gating**: *Not implemented* — the 3-parallel cloud cap was
   intended here but is not enforced; see §4.
4. **Health Monitoring**: Detect if the Mac Gateway is down, so a request that
   decided `local` fails closed with `503` rather than reaching the cloud.

## 2. Routing Algorithm

The decision is one function of three inputs — `requiredCapability(privacy,
latency_class, model)` — evaluated in this priority order:

1. **Model locality** (hardest, below only privacy). A model Ollama hosts in the
   cloud cannot be served by the Mac at all, so naming one is a request for the
   cloud even when the client sends no routing fields. See §2.1.
2. **Privacy** (hard):
   - `LOCAL_ONLY` → local. If no Mac gateway is healthy → `503 Service
     Unavailable` (fail closed).
3. **Latency class** (a preference, and the only input with a default):
   - `interactive` → cloud.
   - anything else, including absent → local.

Then the target is resolved:

- **Cloud**: the cheapest provider advertising the `cloud` capability wins
  (`CostPerToken`), so adding a cheaper cloud provider takes the traffic without
  a code change.
- **Local**: the first healthy gateway advertising `local`.

**There is no fallback from local to cloud.** A request that decided `local` and
finds no healthy gateway returns `503 No healthy local gateway found`; it is
never quietly re-routed to the cloud. Earlier drafts of this section described a
fallback — that was never implemented, and implementing it is what `LOCAL_ONLY`
exists to prevent.

**Concurrency is not gated.** §4 describes a cap that does not hold; see the note
there.

### 2.1 Locality from the model tag

`isCloudModel(model)` reads the marker Ollama puts in the tag: the substring
after the **last** `:` is either `cloud` or `<size>-cloud`.

| model | cloud? |
|---|---|
| `deepseek-v4.1-flash:cloud` | yes |
| `nemotron-3-nano:30b-cloud` | yes |
| `namespace/gpt-oss:120b-cloud` | yes |
| `granite4:3b`, `hermes3:8b` | no |
| `local-brain` (a capability alias) | no |
| `mystery-cloud`, `foo:cloudy` (no tag / wrong tag) | no |

**An untagged name is never cloud**, and that asymmetry is deliberate. The marker
is the only locality signal a client can carry: an OpenAI-SDK agent names a model
and sends no routing envelope. But guessing "this looks cloudish" risks sending a
*local* model to the cloud, which `LOCAL_ONLY` forbids, whereas declining to
guess can only send a cloud model to the Mac, where it fails loudly with its own
name in the error. Failing loudly at the local target is the safe direction.

**The one irreconcilable combination is refused.** `LOCAL_ONLY` naming a
cloud-hosted model cannot be satisfied by either target, so it returns `409
Conflict` naming the model:

```
LOCAL_ONLY cannot be served by the cloud-only model "minimax-m3:cloud"
```

It is never promoted to cloud (the privacy rule), and never re-routed to a Mac
that does not have the model (an anonymous 404).

## 3. Capability Registry

A map of model *aliases* (capabilities) to their local and cloud model names:

- `local-brain` $\rightarrow$ `{local: granite4:3b, cloud: gemma4:31b-cloud}`
- `local-coder` $\rightarrow$ `{local: hermes3:8b, cloud: deepseek-v4-pro:cloud}`

The alias selects a capability (brain vs coder); the routing decision then picks
the local or cloud model name for that capability.

### 3.1 Model resolution: alias or literal

`resolveModel(model, target)` maps the client's `model` field to the name sent to
the target, in one of two modes:

| input | mode | behaviour |
|---|---|---|
| a **capability alias** (a key in the map above) | policy chooses | resolved per target — the alias's `local` name when the target is the gateway, its `cloud` name when the target is cloud |
| **anything else** | literal passthrough | returned **verbatim** as `model_name` |

**The guard: `model_name` is never empty.** If the client named something, that
name is what reaches the target, and the target's own error is the report. On the
local path an unknown model produces Ollama's `404`, surfaced as a `500` carrying
the model id, so telemetry shows the name that failed. An earlier version looked
the name up in the capability map and used the zero value on a miss, so a literal
model arrived empty and failed *anonymously*. The bug was never that a bad name
fails — it is that it failed with nothing naming it.

> **Why no name→capability table.** It would have to be maintained, would lose
> the client's exact model identity, and would break the moment either side
> renames a model. Passthrough plus a real error is smaller and truer.

### 3.2 `GET /capabilities`

Control serves the client-facing namespace — the capability aliases, sorted by
id — together with the context ceiling the gateway will honour:

```json
{
  "context_length": 32768,
  "capabilities": [
    { "id": "local-brain", "local": "granite4:3b", "cloud": "gemma4:31b-cloud" },
    { "id": "local-coder", "local": "hermes3:8b", "cloud": "deepseek-v4-pro:cloud" }
  ]
}
```

The frontend reads this to build `GET /v1/models`
([`lattice-frontend.md`](lattice-frontend.md) §2.2), so the gateway's real
ceiling is discoverable rather than invisible.

### 3.3 Gateway context ceiling

Control stores `gatewayMaxContext`: the ceiling the gateway advertises. It is
refreshed on each 10-second health poll from the gateway's `/health` payload
(`max_context`), because the gateway — not Control — is the authority on what a
context window costs in KV cache on that hardware
([`lattice-gateway.md`](lattice-gateway.md) §3.2). `gatewayMaxContext` is what
`GET /capabilities` reports as `context_length`.

## 4. Concurrency Management

**Recorded drift: the 3-parallel cloud cap is not enforced.** The intent was to
gate cloud dispatch at three in flight and spill the fourth to the local
gateway. What the code does is increment `cloudActive`, hand the decision to a
*pre-allocated buffered* response channel, and decrement it again — a send that
never blocks, so `cloudActive` is back to zero before the frontend has started
proxying and a cap of three can never be reached. `GET /status` therefore
reports `"cloud_active": 0` at all times, including while cloud requests are in
flight. The `semaphore` channel in `dispatcher()` has the same shape and is
released for the same reason.

Consequence: cloud concurrency is bounded only by whatever the cloud endpoint
does. The counter is a working gauge of nothing, and the spill-to-local path it
was meant to trigger does not exist. Implementing it needs a design change —
the decision would have to be handed out *after* a slot was secured, which
means the frontend's proxy call has to be inside the gate, not outside it.

## 5. Implementation Plan

- **Language**: Go (Single binary).
- **API**: A simple HTTP server that acts as the "Lattice Frontend".
- **State**: In-memory for concurrency and health; simple JSON file for capability registry.
- **Exit Test**:
  - Send an `interactive` request $\rightarrow$ Route to Cloud.
  - Send a `batch` request $\rightarrow$ Route to Mac.
  - Send 4 concurrent `interactive` requests $\rightarrow$ 3 go to Cloud, 1 is queued/spilled. (**plan-ahead:** the cap is not enforced, so all 4 go to Cloud — §4.)
  - Mock Mac down $\rightarrow$ Route `LOCAL_PREFERRED` to Cloud. (**plan-ahead:**
    `LOCAL_PREFERRED` is not implemented — the policy table routes only
    `LOCAL_ONLY` to the local gateway with no fallback, and everything else by
    latency class — so this test cannot pass today.)

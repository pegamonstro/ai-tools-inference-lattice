# Spec: Lattice Control (Phase 2)

**Status:** Draft
**Host:** RPi4

The Lattice Control plane is the intelligence of the system. It does not execute inference; it performs the **routing decision**.

## 1. Core Responsibilities

1. **Routing Decision**: Map an `inference.v1` request to a specific execution target (Mac Gateway or Cloud Provider).
2. **Capability Registry**: Track which models are resident on the Mac and which are available in the cloud.
3. **Concurrency Gating**: Enforce the 3-parallel cap on cloud requests.
4. **Health Monitoring**: Detect if the Mac Gateway is down and trigger fallback to cloud.

## 2. Routing Algorithm

When a request arrives at the Control plane:

1. **Privacy Gate** (Hard):
   - If `privacy == LOCAL_ONLY` and Mac Gateway is unhealthy $\rightarrow$ return `503 Service Unavailable` (Fail Closed).
2. **Latency/Resource Path**:
   - If `latency_class == interactive`:
     - Attempt route to **Cloud**.
     - Check Cloud Concurrency: If current parallel cloud calls $\ge 3$, queue the request or spill to **Local (Mac)** if privacy allows.
   - If `latency_class == batch`:
     - Route to **Local (Mac)**.
3. **Fallback**:
   - If target is Mac and Mac is unhealthy $\rightarrow$ route to Cloud (if privacy allows).
4. **Decision**:
   - Return the Target Endpoint + Model Mapping.

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
  "context_length": 65536,
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

The Control plane maintains a counter for active cloud requests.
- `Increment` on dispatch to cloud.
- `Decrement` on response from cloud.
- Use a simple Go channel or atomic counter.

## 5. Implementation Plan

- **Language**: Go (Single binary).
- **API**: A simple HTTP server that acts as the "Lattice Frontend".
- **State**: In-memory for concurrency and health; simple JSON file for capability registry.
- **Exit Test**:
  - Send an `interactive` request $\rightarrow$ Route to Cloud.
  - Send a `batch` request $\rightarrow$ Route to Mac.
  - Send 4 concurrent `interactive` requests $\rightarrow$ 3 go to Cloud, 1 is queued/spilled.
  - Mock Mac down $\rightarrow$ Route `LOCAL_PREFERRED` to Cloud. (**plan-ahead:**
    `LOCAL_PREFERRED` is not implemented — the policy table routes only
    `LOCAL_ONLY` to the local gateway with no fallback, and everything else by
    latency class — so this test cannot pass today.)

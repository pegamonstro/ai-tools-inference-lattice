# Spec: Gateway Registry — announcement, unified routing, adequacy

**Status:** Approved (incremental implementation)
**Host:** RPi4 (Control Plane) / Mac (Gateway)
**Gate:** none

This spec redefines how the Control Plane and the Gateways relate. It supersedes
the routing and registry sections of [`lattice-control.md`](lattice-control.md)
and [`lattice-gateway.md`](lattice-gateway.md), and it is the implementation
target that the MLX-provider work (`lattice-mlx-provider.md`) was the first step
toward.

## 1. The redefinition

Two roles, stated once and used everywhere:

1. **A Gateway is a content-agnostic executor.** It hosts multiple models and
   multiple providers (Ollama, MLX), and it *announces* three things about
   itself: its **models**, its **capabilities**, and its **slots**. It never
   decides *what* runs where — it translates a request to the right provider and
   executes it. The Control Plane owns policy; the Gateway owns fact.

2. **The Control Plane is an orchestrator.** It receives inference requests from
   applications, resolves the **inference type** through the **workflow**
   (privacy → capability → budget → latency), and delegates to a Gateway that is
   *available and adequate* — healthy **and** hosting the resolved model **and**
   with a free slot — not merely the first healthy node that happens to share a
   capability string.

The change collapses the Control Plane's two hardcoded registries (`providers`
for cloud, `gateways` for local) into **one registry populated by
announcement**, and replaces "first match" routing with **filter → score**.

## 2. Current state (what is hardcoded today)

`cmd/lattice-control/main.go` registers three things at startup, in code:

- **`providers`** — `ollama-cloud-primary`, `ollama-cloud-secondary`
  (`capabilities: ["cloud"]`, cost + rate-limit). Source: `init()`, lines 85–98.
- **`gateways`** — `mac-gateway` (`capabilities: ["local"]`). Source: `init()`,
  lines 100–104.
- **`capabilities`** — the client-facing alias map (`local-brain` →
  `granite3-moe:3b` local / `gemma4:31b-cloud` cloud). Source: lines 123–132.

Routing (`handleRoute`, lines 644–690) has two disjoint branches: the cloud
branch iterates `providers` for the cheapest `cloud`; the local branch iterates
`gateways` for the first healthy entry whose capability is `local`. Neither
branch checks whether the target *hosts the resolved model* or *has a free
slot*.

The Gateway already announces `max_context` and `providers` on `/health`
(`cmd/lattice-gateway/main.go`, `handleHealth`), and the Control Plane already
polls and stores them (`monitorHealth`, lines 155–187; `gatewayProviders`,
line 208) — but nothing in the routing decision consumes that announcement. This
spec makes the announcement authoritative instead of decorative.

## 3. Target model

### 3.1 One gateway type, one registry, two sources

The `Provider` and `Gateway` structs collapse into one:

```go
type Gateway struct {
    ID           string
    Endpoint     string
    Capabilities []string // locality ("local","tiny","cloud") + modality (below)
    Models       []string // concrete models this gateway serves; nil/empty = wildcard
    Slots        int      // local concurrency ceiling (0 = not a slot-limited node)
    CostPerToken float64  // cloud only
    RateLimit    int      // cloud only, requests/min
}
```

`cloud` vs `local` becomes a *capability bit*, not a separate registry. The
registry is `map[string]Gateway`, built from **two sources**:

- **Announced** — local gateways self-describe on `/health`; the Control Plane's
  existing 10 s poll rebuilds their entries. This is the only discovery path and
  requires no new infrastructure: liveness and capacity come free from the poll.
- **Declared** — cloud endpoints are *declared* in Control Plane config, not
  announced. A cloud subscription has no self-description protocol for the
  Control Plane to poll, and its "capacity" is a rate limit and a cost, not a
  slot count. It is the same `Gateway` type and the same routing path; only its
  *source* differs.

The `LATTICE_GATEWAY_URL` / `LATTICE_OLLAMA_URL` endpoints stay where they are;
only how a target is *chosen* changes.

### 3.2 The announcement (gateway `/health`)

The Gateway's `/health` payload becomes:

```json
{
  "status": "ok",
  "max_context": 32768,
  "capabilities": ["local", "tiny", "chat", "embeddings", "tool_calling"],
  "slots": 1,
  "models": ["granite3-moe:3b", "qwen2.5-coder:3b", "embeddinggemma"]
}
```

- `models` is the **flattened** union of local models across the Gateway's
  providers. The provider split (`ollama` vs `mlx`) stays internal to the
  Gateway — it is how the Gateway *executes*, not something the Control Plane
  routes on, so it is not announced. (This replaces the current `providers`
  map, which was an intermediate step.)
- `capabilities` is the Gateway's self-description: locality is intrinsic to the
  host (`local`, `tiny`), modality is derived from the provider config (MLX is
  chat-only; Ollama adds `embeddings` and `tool_calling`).
- `slots` is the **static** concurrency ceiling (see §3.4).

### 3.3 Capability vocabulary

Capabilities are an open set of strings, not an enum, so new axes are additive:

| axis | values | meaning |
|---|---|---|
| locality | `local`, `tiny`, `cloud` | where inference is computed |
| modality / inference type | `chat`, `embeddings`, `tool_calling`, `vision`, `reasoning` | what the gateway can serve |

The locality axis is what the router filters on today; the modality axis is how
"inference types and workflows" (§1 point 2) is expressed. **Implemented now:
`chat` and `embeddings`.** The rest (`tool_calling`, `vision`, `reasoning`, and
any future taxonomy) are reserved bits the vocabulary already admits without a
protocol change — a gateway that does not yet support one simply omits it, and a
request that requires it fails to match any adequate gateway, loudly.

### 3.4 Slots = static capacity, not availability

The Gateway announces a **static** ceiling (`slots: 1` for the Mac — one
resident local model at a time). It does *not* report "I am free right now";
that is the Control Plane's job, because the Control Plane sees every request it
has dispatched. Availability is derived:

```
availability(gateway) = gateway.Slots − inflight[gateway]
```

`inflight` is a `map[string]int` the Control Plane increments on dispatch and
decrements on completion. The Gateway keeps its own internal single slot (the
existing `chan struct{}, 1` serialisation) as a backstop; the Control Plane's
tracking is the *scheduling* layer that stops it from queueing a second request
on an already-busy gateway when another adequate one exists.

Cloud gateways have `Slots: 0` and instead expose `RateLimit`; their "free slot"
test is rate-limit headroom, not a concurrency count.

### 3.5 The model registry is two resolutions deep

The hardcoded alias map does not disappear; it changes meaning. There are two
distinct questions, answered by two distinct authorities:

1. **Alias → concrete model** (`local-brain` → `granite3-moe:3b`). This is
   *policy*, and policy lives in the Control Plane — it stays config, exactly as
   today's `capabilities` map does. "RPi4 decides."
2. **Concrete model → gateway** (`granite3-moe:3b` → which healthy gateway hosts
   it, with capacity). This is *fact*, and fact is announced — the Control Plane
   reads it from the registry it built from `/health`. "Mac (and cloud) report."

So a request resolves in two steps: the Control Plane maps an alias (or a
literal model name) to a concrete model from its config, then selects an
adequate gateway that actually hosts that concrete model from the announced
registry. The alias map stops hardcoding a *string that may not be served*; it
hardcodes a *preference*, and the registry verifies it against reality.

### 3.6 Adequacy routing: filter, then score

One routing path replaces the two branches. Given the resolved concrete model
and the required capability:

**Filter** — drop any gateway that fails any of:
- not healthy (per the health poll);
- does not carry the required capability (`local` for `LOCAL_ONLY`, etc.);
- does not host the concrete model (unless `Models` is empty, the cloud
  wildcard);
- has no free slot (or, for cloud, no rate-limit headroom).

**Score** — among survivors:
- cloud → lowest `CostPerToken`;
- local → fewest `inflight`, tie-broken by first healthy.

**Fail loudly.** If nothing survives, refuse. `LOCAL_ONLY` is never quietly
promoted to cloud, and a model no gateway hosts is never quietly rerouted to a
gateway that does not have it — the current refusal semantics (`requiredCapability`
returning an error, `StatusConflict`/`ServiceUnavailable`) are preserved, and
the failure now also fires when the *registry* lacks an adequate host, not only
when the policy is irreconcilable.

## 4. Not in scope

- **Push registration** (gateway `POST /register` + heartbeat + lease). Rejected:
  it reinvents service discovery, and the existing poll already provides liveness
  and capacity. This is the "no new infrastructure" constraint applied.
- **New inference types beyond `chat` / `embeddings`.** The vocabulary admits
  them; nothing is implemented here for `tool_calling`, `vision`, or
  `reasoning` beyond announcing the bit where a provider already supports it.
- **Multi-gateway load-balancing in production.** The design expresses
  filter → score, but with one local gateway (the Mac) and two cloud
  subscriptions the "score" step is trivial today. It exists so a second local
  gateway (e.g. the RPi4-internal gateway of the frozen topology) slots in
  without a routing rewrite.
- **The Gateway's internal provider mechanics.** The MLX/Ollama provider
  registry, model→provider mapping, and single-slot serialisation are unchanged;
  only what the Gateway *announces* about them changes.

## 5. Incremental implementation order

Each step lands as a working, committed change; the order keeps the router
correct at every point.

1. **Gateway announces the new shape.** Add `capabilities` and `slots` to
   `/health`, flatten `providers` into `models`. Keep `max_context`. No consumer
   breaks — routing still ignores the announcement.
2. **Control Plane reads the new shape.** Extend `monitorHealth`'s decode to
   capture `capabilities`, `slots`, and `models`, and store them per-gateway.
3. **Unify the registry.** Replace the two `init()` registries with one
   `map[string]Gateway`; cloud entries become declared `Gateway`s with
   `Capabilities: ["cloud"]` and empty `Models`; local entries are rebuilt from
   the announcement. `localityFor` derives from the entry's capabilities rather
   than which map it lived in.
4. **Adequacy routing.** Replace the two-branch `handleRoute` selection with the
   filter → score function of §3.6, driven by `inflight`. `resolveModel` keeps
   resolving alias → concrete model; the registry now answers concrete model →
   gateway.
5. **Telemetry and tests.** Extend the routing telemetry to name the chosen
   gateway's adequacy inputs (capability, slot) and add table-driven tests for
   the filter → score function (hosts-model, free-slot, capability-mismatch,
   wildcard-cloud, cheapest-cloud, fewest-inflight-local).

## 6. Decision log

- **2026-09-30 — unify registries.** `providers` + `gateways` collapse into one
  `Gateway` type; cloud vs local is a capability bit. Source: the redefinition
  "gateways are content-agnostic and handle multiple models and multiple
  providers."
- **2026-09-30 — two-resolution model registry.** Alias → concrete model is
  Control Plane config (policy); concrete model → gateway is announcement
  (fact). This is what keeps "RPi4 decides, Mac reports" intact under the
  redefinition.
- **2026-09-30 — pull, not push.** Announcement via the existing `/health` poll,
  no registration protocol, per the standing "no new infrastructure" rule.
- **2026-09-30 — slots are static capacity.** Availability is the Control
  Plane's derived `Slots − inflight`, matching how K8s, LiteLLM, and the
  inference-server routers separate announced capacity from tracked occupancy.
- **2026-09-30 — inference types are an open set.** `chat` + `embeddings`
  implemented now; `tool_calling`/`vision`/`reasoning` admitted as capability
  bits with no protocol change.

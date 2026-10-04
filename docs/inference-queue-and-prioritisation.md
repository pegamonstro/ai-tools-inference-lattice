# Inference Queue, Prioritisation & Monitoring — Scoped Design

> Status: **research / scoping** — not implemented. This documents the current
> admission model and scopes a robust inference job queue for the gateways.
> Decisions marked **"Your call"** are open for the owner to resolve before any
> implementation.

## 1. What exists today

The gateway already has an admission layer, so this is an *extension*, not a
greenfield queue.

- **Slots = resizable counting semaphore.** `slotSemaphore` in
  `cmd/lattice-gateway/main.go` (a `sync.Cond`-based counter — a fixed channel
  can't resize). `inferenceSlots` starts at 1, `maxSlots` is 3
  (`LATTICE_GATEWAY_MAX_SLOTS`); the limit is recomputed on every `/health`
  poll by `computeSlots()` (smallest-first greedy fit of announced model sizes
  into `budgeter.availableBytes()`).
- **`acquire(ctx)` blocks until a slot frees or the client cancels.** So
  concurrency is *already queued*, FIFO, with **no priority** and **no
  queue-depth counter**.
- **Hard rejections that should be queueing instead:**
  - memory-pressure `429` in `handleImage`/chat/embed/speech (checked *before*
    slot acquisition), and
  - the mflux sidecar's single-flight `409 {"error_type":"busy"}` — the
    gateway serialises first via its slot, but the 409 still surfaces to
    clients when the sidecar is independently busy.
- **Priority exists only in the control plane, only for cloud, and is
  broken.** `highPriorityQueue`/`lowPriorityQueue` (buffered chan, cap 100)
  fed by `latency_class == "interactive"`, drained by `dispatcher()` behind a
  `semaphore(3)`. `cloudActive` reads 0 (the design doc itself records it),
  so the cloud 3-parallel gate is unenforced. **Local requests bypass the
  queue entirely** (synchronous return). There is no per-application identity,
  quota, or fair scheduling.
- **Lifecycle is fully synchronous, blocking HTTP** end-to-end (frontend is a
  `httputil.ReverseProxy`). No job id + poll, no cancel/inspect endpoint, no
  persistence across restart.
- **Monitoring is offline.** Three append-only JSONL streams
  (`telemetry-{gateway,control,frontend}.jsonl`) plus `lattice-stats`, an
  offline CLI that parses a stale schema (expects `total_time_s`, doesn't
  match the gateway's `elapsed_s`). No live metrics endpoint, no Prometheus,
  no queue-depth or wait-time metric.

Topology for context: Frontend (RPi4) → Control (RPi4) → Gateway (Mac).
Control holds a `gateways` registry, health-polls each `/health` (10s), and
picks the lowest-priority gateway with `Slots > 0`. Gateways are otherwise
independent — no shared queue or cross-gateway load-balancing.

## 2. Target requirements

A "robust inference queue on the gateways" means, concretely:

1. **Queue, don't reject** — concurrent requests from multiple apps wait with a
   bounded wait, rather than 409/429.
2. **Prioritisation** — interactive beats batch, with starvation protection.
3. **Management** — inspect queue depth/position, cancel, drain.
4. **Monitoring** — live queue depth, wait time, per-model latency/throughput;
   Prometheus-compatible `/metrics`.
5. **Fairness across applications** — per-app identity + optional concurrency
   quota, so one noisy app can't starve the others.

Durability across restart and cross-gateway coordination are explicitly
**deferred** (see D1 / Phase 3).

## 3. Decisions & recommendation

### D1 — Queue substrate: in-process vs broker

| Option | Cost | Notes |
|---|---|---|
| **In-process stdlib heap** *(recommended)* | none | Matches the stdlib-only `go.mod`; single gateway today; extend the existing semaphore. |
| asynq (Redis) | +Redis | Weighted priority queues, retries, scheduler, Asynqmon UI, Prometheus metrics. Highest throughput. |
| River (Postgres) | +Postgres | Durable, transactional `InsertTx`; Postgres already runs on the host. |

**Recommendation:** in-process now. The lattice is Go stdlib-only and
single-gateway-per-model; a broker only pays off when you need *cross-gateway*
sharing or durability — that's Phase 3, and asynq/River remain drop-in options
then.

### D2 — Scheduling policy

Mirror vLLM's priority scheduler: a **min-heap waiting queue** ordered by
priority (lower = higher), **FCFS tiebreaker** on arrival time, plus an
**aging factor** so low-priority requests aren't starved. Strict priority
alone is the classic mistake — a steady interactive stream would starve batch
forever. Aging bounds the wait.

### D3 — Admission: queue vs reject

Keep the semaphore as the *executor*, add a **bounded priority queue** in front
of it with a max depth + max wait. Reject (`429`) only on queue-full. Memory
pressure should keep driving the slot *limit* (it already does via
`computeSlots`), not hard-reject individual requests — let the queue absorb a
transient spike.

### D4 — Job model: sync vs async

Hybrid. Interactive chat stays synchronous (low latency, matches today's
frontend). Long-running work — **image generation and batch** — gets an async
`job_id` + poll/SSE endpoint, exactly like `img-gen`'s existing queue. The
mflux 409 becomes a *queue* on the gateway, never a client-facing error.

### D5 — Application identity & quota

Add a minimal `X-App-Id` header (frontend already mints a `RequestID`; extend
it). Derive a per-app default priority and an optional per-app
`max_concurrent` cap. No auth — this is a trusted-homelab control, not an
access-control boundary.

### D6 — Monitoring

Add a **queue-depth counter and wait-time histogram** to the semaphore (the
cond-based queue currently tracks neither). Expose a Prometheus-text
`/metrics` endpoint on the gateway and control (queue depth, wait time,
per-model request latency, throughput, slot usage, rejection count). Retire the
stale `lattice-stats` schema or repoint it at `/metrics`.

## 4. Phasing

- **Phase 1 (core):** priority queue in front of the semaphore + queue-depth
  metric + `/metrics` endpoint. Replaces 409/429 with bounded waits. Small,
  self-contained, highest value.
- **Phase 2:** `X-App-Id` + per-app quota; async job id + poll/SSE for image
  and batch (reuses img-gen's queue/SSE pattern); cancel/drain.
- **Phase 3 (optional):** external broker (asynq/River) if cross-gateway
  sharing or durable jobs become real requirements.

## 5. Your calls

1. **Substrate** — stay stdlib in-process, or bring in Redis (asynq) / Postgres (River)?
2. **Priority** — strict priority, or weighted + aging (recommended)?
3. **Job model** — hybrid sync+async now, or queue-first with async later?

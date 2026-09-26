# Spec: Build Orchestration — macro orchestrator, local workers

**Status:** Draft
**Hosts:** RPi4 (orchestrator + workers), Mac (local inference via the Gateway)
**Depends on:** [lattice-agent-surface.md](lattice-agent-surface.md),
[inference-v1.md](inference-v1.md), [lattice-gateway.md](lattice-gateway.md)
**Date:** 2026-09-26

Turn Hermes — already resident on the RPi4 and already routing all of its
inference through Lattice — into a **build system**: a strong cloud model
decomposes work into units small enough for a weak local model, and the local
model does the mechanical work. Lattice is not extended to do this. It is used,
and the one property that makes it usable is that **its routing rule is the
model name**.

---

## 1. The strategy, stated precisely

The proposition under test was: *the orchestrator scaffolds and validates the
macro; the local models validate the micro.*

That is **half right, and the wrong half matters.** Evidence in §6. Corrected:

- The orchestrator **scaffolds** the macro (decomposes) and **validates the
  plan**, not the workers' output.
- Local models **do** the micro work — mechanical, single-context, gate-checked
  units.
- Local models **never verify.** A weak model verifying its own family's output
  is the weakest available signal: verification gains shrink when solver and
  verifier share a family, and a weak verifier's own test encodes its own
  misreading of the spec.
- Deterministic gates verify the **mechanical** properties. A cloud review
  verifies the **semantic** ones. Neither substitutes for the other.

## 2. Why this is cheap: the measured economy

| quantity | value | source |
|---|---|---|
| Hermes harness tax — prompt tokens per turn, before any task text | ~12,925 | measured on the input `"Say OK"` |
| local turn wall-clock at that tax, gateway-chosen 16K context | ~220 s | same measurement |
| local prefill rate warm | ~10 tok/s | `docs/baseline.md` |
| **local concurrency** | **1** | Gateway `semaphore=1` + RAM margin |
| local context ceiling | 32,768 | agent-surface §6 (65536 measured at 13 GB RSS / 2.2 GB swap, rejected) |
| orchestrator turn (cloud, this prototype) | 109.6 s | telemetry, `kimi-k2.7-code:cloud` |

Three consequences follow, and they are the whole design:

1. **The harness tax is fixed; the brief is the variable.** Every worker pays
   ~13K tokens of prefill whatever it is asked. Shrinking the *task payload*
   is therefore the highest-leverage optimisation available — far more than
   tuning which target serves it.
2. **Local is one-wide.** Workers queue inside the Gateway, so a unit is the
   unit of wall-clock cost. A ten-unit plan is ten serial local turns. Units
   that need a multi-turn loop are the wrong shape for local.
3. **Local prefill is free but slow; cloud prefill is fast but billed.** Put
   *volume* on local and *judgment* on cloud. That is the hybrid split
   `docs/lattice-design.md` §4 already describes — this spec is its
   application to build work, not a new policy.

## 3. Design

### 3.1 Topology

```
cloud orchestrator (one-shot, judgement, billed)
        │  emits unit-N.md briefs
        ▼
Lattice frontend :8080  ──►  control: routes by model tag
        │
        ├──► mac-gateway           (local workers, serialised, free)
        └──► ollama-cloud-secondary (escalation, parallel, billed)
```

### 3.2 The router is the model tag

No routing code is written. `isCloudModel()` in `cmd/lattice-control/main.go`
sends an untagged model name to local and a cloud-tagged one to cloud. A worker
pinned with `-m local-brain` is a local worker; `-m kimi-k2.7-code:cloud` is a
cloud worker. Verified: both Hermes providers (`ollama-launch`, `ollama-mac`)
resolve to `http://127.0.0.1:8080/v1` — **the Lattice frontend** — so a pinned
worker cannot bypass Lattice even by accident.

### 3.3 Worker invocation

```sh
hermes chat --query-file - --oneshot -Q \
  -m <model> --provider ollama-launch \
  --max-turns 1 --run-budget <seconds>
```

- `--query-file -` reads the brief from stdin; nothing is shell-interpreted, so
  quotes and `$(...)` survive verbatim.
- `--oneshot -Q` answers and exits — no interactive session, no banner.
- `--max-turns` / `--run-budget` bound a weak model that would otherwise loop.
- The worker is a **fresh process**. It inherits no conversation, no plan, no
  sibling results. Its entire context is its brief.

### 3.4 Unit protocol

A unit is the smallest thing that carries its own gate. Each unit has:

| field | content |
|---|---|
| Files | exact paths; for existing files, the functions or line ranges touched |
| Consumes | exact names/types/signatures used from earlier units |
| Produces | exact names/types/signatures later units rely on |
| Verify | the exact command that proves it done, and its expected result |
| Worker prompt | ≤120 words, self-contained, the whole instruction the worker sees |

The brief goes to `unit-N.md`; the worker writes code plus `unit-N-report.md`.
Units are handed over as **files, never as conversation**.

### 3.5 Gates

Cheap, deterministic, mechanical:

- `go build ./...` — compiles
- `go test ./...` — passes
- `gofmt -l` — clean

A gate is an **exit code**, never a model's opinion. Gates catch mechanical
defects. They cannot catch a wrong reading of the spec (§6), which is why §3.6
exists.

### 3.6 Review — the part the prototype proved necessary

- **Plan review, once, cloud, before any worker runs.** The orchestrator's own
  decomposition is reviewed against the spec by a different, stronger model
  than the one that wrote it. This is the only net for interpretation errors.
- **Per-unit diff review, cloud, where the unit's semantics matter.** Not for
  every unit — for units whose correctness is a *reading* of the spec rather
  than a compile.

### 3.7 Escalation

A failed gate re-dispatches **the same brief** with a higher model tag:

```
local-brain  →  local-deep  →  cloud-coder
```

Same brief, different `-m`. Escalation is the routing mechanism already built —
no retry daemon, no queue, no scheduler.

## 4. Non-goals

Anti-drift rule 5 ("no speculative infrastructure") is binding here.

- **No DAG engine, scheduler daemon, job queue, state DB, or message bus.** The
  orchestrator is a one-shot process; state is files on disk.
- **No parallel local workers.** Local is one-wide by construction. Plans that
  assume otherwise are wrong, not merely slow.
- **No change to Lattice's routing policy.** The cloud-locality rule and the
  `LOCAL_ONLY` gate are already the correct behaviour for this use.
- **No local verification.** See §1.

## 5. Prerequisites (verified, not assumed)

Each was checked against a running system on 2026-09-26, not inferred.

1. **`/v1/embeddings` is not served.** Through the frontend it returns
   `404 page not found`. Hermes's `auxiliary.embedding` points at that endpoint,
   and since both Hermes providers are the frontend, **no direct path to local
   inference exists**. Embedding underpins memory/recall, which the orchestrator
   needs. Either the frontend and gateway gain an embeddings passthrough, or
   `auxiliary.embedding` is repointed and memory is disabled. *Unresolved.*
2. **No token fields are emitted.** Neither `telemetry-control.jsonl` nor
   `telemetry-frontend.jsonl` carries `prompt_tokens`, `completion_tokens`, or
   any equivalent. Spec'd token accounting is **unmeasurable**, and any tool
   claiming it today would report zero. This is a prerequisite for
   [lattice-observability.md](lattice-observability.md) §2, not for this spec's
   orchestration — but the orchestrator's economy model (§2) is estimated from
   one measurement until it is fixed.
3. **The model aliases must exist on the target.** Lattice advertises
   `local-brain` and `local-coder` at `context_length: 32768`. Hermes's
   `local-brain` alias resolves to `llama3.2:3b` via `ollama-mac`, and that name
   is **not** in that provider's `models` list — a config inconsistency to
   verify before a worker is pinned to it.

## 6. Evidence: the prototype, and what it says

One orchestrator turn (`kimi-k2.7-code:cloud`, telemetry-confirmed target
`ollama-cloud-secondary`, 109.6 s) decomposed a real task: *bring
`cmd/lattice-stats/main.go` into conformance with observability §2*.

The task was chosen because §2 requires "total token spend (Cloud)" and that
quantity **does not exist in the input** (§5.2). It is a trap: the plausible
wrong answer compiles and reports zero forever.

**It found the trap.** Its "Blocked requirements" section states the token
requirement cannot be delivered from that file alone and depends on an upstream
telemetry change. It also caught the two-layer schema divergence and honoured
the determinism constraint by sorting in the stats engine. Units were sized at
20–40 minutes of a human's attention each, with exact signatures and
self-contained prompts.

**It also produced a semantic defect.** Its parser maps control's
`decision_time_s` onto `TotalTime`, and its aggregator then averages `TotalTime`
per target. But `decision_time_s` is *routing* time (~0.002 s) and the
frontend's `total_time_s` is *end-to-end* (~2 s). Pooling them reports ~0.9 s
for `mac-gateway` — a figure true of neither layer. Additionally `Layer` and
`ExecutionTime` are produced and never consumed.

**The conclusion.** Mechanical decomposition quality holds up. Semantic
interpretation does not. `go test` cannot catch the defect above, because the
worker would write a test encoding its own misreading and it would pass. Hence
§3.6: the plan review is not ceremony, it is the only working net.

## 7. Invariant check

Against `docs/lattice-design.md` §6:

| rule | status |
|---|---|
| 2. RPi4 decides, the Mac executes | **held** — the orchestrator runs on the RPi4, all local inference on the Mac |
| 5. No speculative infrastructure | **held** — no new components; §4 is the anti-drift clause |
| 6. No swap thrashing | **held** — workers go through the Gateway's single slot; serialisation is the protection |
| 7. Deterministic infrastructure | **held** — one-shot processes, file-based state, exit-code gates |
| 8. Every phase has an exit test | **held** — §6 is this phase's, and it has been run |

## 8. Recorded drift

- `docs/lattice-design.md` §4 claims "10 slow local workers in parallel ≈ 1
  fast cloud call's wall-clock". **Local concurrency is 1** (§2). The claim is
  design intent, not implemented behaviour — the same class of drift recorded
  for the fallback table and the 3-parallel cap.

## 9. Open questions

1. Embeddings (§5.1): add passthrough, or repoint and disable memory?
2. Token accounting (§5.2): which layer should emit it — the frontend, which
   sees the full response, or the gateway, which sees it first?
3. Target→locality classification has no rule for an unknown target. Two
   literal names (`mac-gateway`, `ollama-cloud-secondary`) are hardcoded today.
   Should locality become a property the control plane reports, instead of a
   name the tools must recognise?

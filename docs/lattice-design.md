# Lattice — Inference Lattice

**Design continuity doc.** Version 0.1. Distilled from the design conversation (source transcript: `LLM INFERENCE ROUTING FOR HOMELAB/LLM INFERENCE ROUTING FOR HOMELAB.md`) plus environment grounding and one user correction to the routing model.

Status: **Phase 0** (charter). This document is the anti-drift anchor — when in doubt, this file wins.

---

## 1. Purpose

A lightweight distributed inference control and execution system for the three-host homelab. The core idea is a strict separation:

- **Control plane** decides *what* inference should happen (policy, capability, health, fallback, concurrency).
- **Gateway** executes *how* (which provider/model actually runs the request).

The problem it solves is *resource scheduling with hard constraints*, not "which LLM API to call":

| Resource | Speed | Cost | Concurrency |
|---|---|---|---|
| Cloud (subscription) | fast | expensive (burns token budget) | **max 3 parallel** |
| Local LLM (Mac) | slow (sometimes 30min+/turn) | free | **unlimited parallel** |
| Local tiny/embedding (rpi4) | fast enough | free | unlimited |

The router's job is to put latency-critical work on the scarce-fast resource and throughput work on the abundant-free resource, while never violating privacy.

---

## 2. Frozen topology

```
Control Plane (RPi4) ── Orchestrates routing based on capability.
  │
  ├── Gateway 1 (rpi4-internal) ── Local: Tiny / Cloud: Subscription
  └── Gateway 2 (Mac)          ── Local: Substantive LLMs
```

Invariant: **the Control plane decides; the Gateways execute.** Any node can act as a Gateway if it provides the necessary capabilities.

---

## 3. Resource tiers (verified)

1. **Cloud** — rpi4's Ollama `:cloud` subscription models: `deepseek-v4*`, `kimi-k2.x`, `glm-5.x`, `gemma4:31b-cloud` (default), `nemotron-3*`, `gemini-3-flash`, `gpt-oss:120b`, `mistral-large-3:675b`, `qwen3.5`, `minimax-m3`. Fast. Cap: 3 models parallel. Burns token budget fast.
2. **Local (Mac only)** — Ollama: `granite4:3b` (main), `gemma3:4b`, `command-r7b:7b`, `hermes3:8b` (+ embeddings). Slow (~10 tok/s warm; 30min+ cold/long-context). Free.

**Memory Constraint**: Parallelism is bounded by the 16GB RAM of the Mac. The Gateway must implement memory-aware concurrency gating to **avoid swap thrashing**, preventing SSD wear. Local is "parallel" compared to cloud, but not "unlimited."

> **No third tier** (user decision 2026-09-25): rpi4's tiny models (`smollm2`, `qwen2.5:1.5b`, `nomic-embed-text`, `embeddinggemma`) are NOT a routing resource. rpi4 is cloud-only; all local inference — including embeddings — consolidates to the Mac.

---

## 4. Routing model (refined)

> **Correction (2026-09-25):** the original "local-first, cloud-fallback" precedence is wrong for interactive use — local can take 30min+/turn. The precedence must flip on latency class, not be fixed.

Routing is a function of four inputs:

1. `privacy` — hard gate.
2. `latency_class` — `interactive` (user waiting) or `batch` (throughput-bound).
3. `parallelism` — how many independent units are needed concurrently.
4. `cost` — token-budget impact.

### Policy table

| privacy | latency_class | route |
|---|---|---|
| `LOCAL_ONLY` | any | local only; fail closed if unavailable (accept latency) |
| `LOCAL_PREFERRED` | interactive | cloud (fast); local only if cloud budget exhausted and latency tolerable |
| `LOCAL_PREFERRED` | batch | local (free + parallel); cloud only if local unavailable |
| `CLOUD_ALLOWED` | interactive | cloud (fast), subject to 3-parallel cap |
| `CLOUD_ALLOWED` | batch | local preferred (free); cloud if local unavailable or explicit override |

**Fallback:** the only fallback for local is **cloud (rpi4)**. Mac unavailable → `LOCAL_PREFERRED` / `CLOUD_ALLOWED` fall back to cloud; `LOCAL_ONLY` fails closed (never cloud).

**Swap Avoidance**: All local routing is subject to a memory-pressure gate. Requests that would force the Mac into swap must be queued or degraded to cloud (if privacy allows) to preserve SSD health.

### Cloud concurrency gate

Cloud is the scarce resource. The Control plane enforces the **3-parallel cap**: excess concurrent interactive cloud requests are queued; non-interactive excess is degraded to local (if privacy allows).

### Hybrid (local + cloud) split

For a workload with both a latency-critical part and a parallelizable part, split it:

- **interactive reply** → cloud (single fast model);
- **parallelizable background** (indexing, embeddings, batch analysis) → fan out to free local workers.

Local's unlimited parallelism is the compensation for its per-item slowness: 10 slow local workers in parallel ≈ 1 fast cloud call's wall-clock, at zero token cost.

### Measurement (resolved in Phase 0)

Measured (see `docs/baseline.md`): `granite4:3b` ≈ 10 tok/s warm — a 100-token turn ≈ 13s, short-reply-viable. The reported 30min+/turn is the cold-load / long-context / model-thrash case (`OLLAMA_MAX_LOADED_MODELS=1`). Conclusion: cloud is the interactive default; local serves batch, embeddings, and short drafts.

---

## 5. Privacy levels

- `LOCAL_ONLY` — only Mac local inference is acceptable; if unavailable, **fail**; never silently send to cloud.
- `LOCAL_PREFERRED` — prefer local; cloud allowed as fallback/upgrade.
- `CLOUD_ALLOWED` — any provider is acceptable.

Privacy is a hard gate, evaluated before latency/cost.

---

## 6. Anti-drift rules

1. Ollama is an implementation detail.
2. The RPi4 decides; the Mac executes.
3. RPi3 remains a security appliance.
4. One canonical protocol (`inference.v1`).
5. No speculative infrastructure (Kubernetes, Redis, Kafka, RabbitMQ, Postgres for routing).
6. **No Swap Thrashing** — the Mac (16GB) is the only local host; the Gateway must prevent memory over-subscription to protect SSD health.
7. Deterministic infrastructure.
8. Every phase has an exit test.
9. Preserve reversibility.
10. Complexity must earn its existence.

---

## 7. Phase roadmap

| Phase | Scope | Exit test |
|---|---|---|
| **0** | Charter, repo, baseline the three hosts | Baseline doc committed; routing thresholds measured |
| **1** | `inference.v1` protocol (OpenAI-compatible envelope + routing metadata) | Spec + reference client/server pass a round-trip test |
| **2** | RPi4 Lattice Control (policy, capability, health, fallback, concurrency gate) | Control answers a routing decision query deterministically |
| **3** | Mac Lattice Gateway (Ollama adapter, concurrency, telemetry) | Gateway serves local inference over `inference.v1` |
| **4** | End-to-end integration | A routed request completes from Control → Gateway → client |
| **5** | Failure & fallback (health checks, circuit breakers, fail-closed LOCAL_ONLY) | Kill a provider; routing degrades per policy, never violates privacy |
| **6** | RPi3 integration (client only) | *(out of scope — this project is strictly rpi4 ↔ Mac)* |
| **7** | Observability & hardening | Metrics/telemetry on; latency/cost/concurrency visible |
| **8** | Provider abstraction | *(implemented — `Provider` interface + registry in the Gateway)* |
| **9** | Advanced scheduling | *(implemented — priority queues: interactive vs batch)* |
| **10** | Lattice 2.x | Post-consolidation review |

Phases 0–9 are implemented. Phase 6 (rpi3) is out of scope. Phase 10 (Lattice 2.x) is a future consolidation review.

---

## 8. Resolved technical decisions

- **Language: Go.** rpi4 has Go 1.24.4 linux/arm64 (no Rust/Node); Mac is arm64. Single static binaries, stdlib `net/http` + `encoding/json`, cross-compiles both targets. Satisfies anti-drift rules 5 & 7.
- **Protocol: `inference.v1`** — an OpenAI-compatible `/v1/chat/completions` envelope extended with routing metadata (privacy, latency_class, parallelism, model hint, `request_id`), over HTTP/JSON. Not gRPC (rule 5, 7).
- **State: SQLite** (Hermes already uses it) or append-only log. Deferred to Phase 2.
- **FOSS routers: rejected for now** (Phase 8). LiteLLM needs Postgres+Redis; OmniRoute has security red flags (hardcoded JWT secret, single maintainer, Socket.dev malware flag); OpenRouter is cloud. All violate at least one anti-drift rule.

## 10. Provider Orchestration & Budgeting

As the Lattice grows to support multiple cloud providers and multiple local gateways, the Control plane evolves from a simple router to an **Orchestrator**.

### 10.1 Multi-Provider Management
Instead of a single cloud endpoint, the Control plane manages a **Provider Registry**. Each provider is associated with:
- **Rate Limits**: (e.g., 10 req/min, 100k tokens/day).
- **Cost Profile**: (e.g., $0.01 / 1M tokens).
- **Capability Set**: (e.g., `supports_reasoning: true`, `supports_vision: false`).

### 10.2 Optimization Logic
The Control plane selects the target based on the following precedence:
1. **Privacy Gate**: `LOCAL_ONLY` $\rightarrow$ Local only.
2. **Capability Match**: Does the provider support the requested `reasoning_effort` or `vision`?
3. **Budget/Rate Check**: Is the cheapest provider currently under its rate limit?
4. **Latency Class**: If `interactive`, prioritize the provider with the lowest measured latency, even if slightly more expensive.

### 10.3 Parameter Translation
The Gateway acts as a translation layer. It takes the `provider_params` from the `inference.v1` request and maps them to the specific API of the chosen provider.
- `reasoning_effort: "high"` $\rightarrow$ (DeepSeek API) `reasoning_effort: "high"`.
- `reasoning_effort: "high"` $\rightarrow$ (Ollama) `num_ctx: 32768` + specific prompt wrapping.

### 10.4 Feedback Loop
The Gateway reports the actual token usage and wall-clock time back to the Control plane via telemetry. This allows the Lattice to dynamically update its "cost per token" and "latency per model" maps, ensuring routing decisions are based on real-time data, not static assumptions.


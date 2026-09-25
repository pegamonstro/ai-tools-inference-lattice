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
RPi4  (user@rpi4)   ── Control plane. Policy, capability registry, lifecycle,
                      health, fallback, observability, cloud-concurrency gate.
                      No substantive local LLM. Cloud client + tiny embedding models.
Mac mac-gateway (this) ── Lattice Gateway. Ollama, provider adapters, concurrency.
                      The ONLY substantive local-LLM host.
RPi3              ── Security appliance (DNS sinkhole, honeypot, bastion).
                      Outside the control plane. Inference client only.
```

Invariant: **the RPi4 decides; the Mac executes.** Neither is allowed to drift into the other's role.

---

## 3. Resource tiers (verified)

1. **Cloud** — rpi4's Ollama `:cloud` subscription models: `deepseek-v4*`, `kimi-k2.x`, `glm-5.x`, `gemma4:31b-cloud` (default), `nemotron-3*`, `gemini-3-flash`, `gpt-oss:120b`, `mistral-large-3:675b`, `qwen3.5`, `minimax-m3`. Fast. Cap: 3 models parallel. Burns token budget fast.
2. **Mac-local LLM** — mac-gateway Ollama: `granite4:3b` (main), `gemma3:4b`, `command-r7b:7b`, `hermes3:8b`. Slow. Free + unlimited parallel.
3. **rpi4-local tiny/embedding** — `smollm2:135m/360m`, `qwen2.5:1.5b`, `nomic-embed-text`, `embeddinggemma`. Embeddings/lightweight only; not "local LLM inference."

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

### Cloud concurrency gate

Cloud is the scarce resource. The Control plane enforces the **3-parallel cap**: excess concurrent interactive cloud requests are queued; non-interactive excess is degraded to local (if privacy allows).

### Hybrid (local + cloud) split

For a workload with both a latency-critical part and a parallelizable part, split it:

- **interactive reply** → cloud (single fast model);
- **parallelizable background** (indexing, embeddings, batch analysis) → fan out to free local workers.

Local's unlimited parallelism is the compensation for its per-item slowness: 10 slow local workers in parallel ≈ 1 fast cloud call's wall-clock, at zero token cost.

### Open measurement (must resolve in Phase 0)

The earlier doctrine recorded `granite4:3b` at ~13.1 tok/s (→ ~40s/responses), but observed reality is 30min+/turn. The entire policy hinges on the true local-latency distribution. **Phase 0 must measure actual local latency** (short vs long context, cold vs warm model) before the routing thresholds are trusted.

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
6. No premature provider abstraction.
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
| **6** | RPi3 integration (client only, behind bastion) | rpi3 requests inference without touching the control plane |
| **7** | Observability & hardening | Metrics/telemetry on; latency/cost/concurrency visible |
| **8** | Provider abstraction (deferred) | Revisit FOSS vs build; only if a real second provider appears |
| **9** | Advanced scheduling (deferred) | Only if observed workloads justify it |
| **10** | Lattice 2.x | Post-consolidation review |

Phases 0–7 are the core. Phases 8–10 are explicitly deferred and gated on observed need.

---

## 8. Resolved technical decisions

- **Language: Go.** rpi4 has Go 1.24.4 linux/arm64 (no Rust/Node); Mac is arm64. Single static binaries, stdlib `net/http` + `encoding/json`, cross-compiles both targets. Satisfies anti-drift rules 5 & 7.
- **Protocol: `inference.v1`** — an OpenAI-compatible `/v1/chat/completions` envelope extended with routing metadata (privacy, latency_class, parallelism, model hint, `request_id`), over HTTP/JSON. Not gRPC (rule 5, 7).
- **State: SQLite** (Hermes already uses it) or append-only log. Deferred to Phase 2.
- **FOSS routers: rejected for now** (Phase 8). LiteLLM needs Postgres+Redis; OmniRoute has security red flags (hardcoded JWT secret, single maintainer, Socket.dev malware flag); OpenRouter is cloud. All violate at least one anti-drift rule.

---

## 9. Open questions

1. **Local latency truth** — resolve via Phase 0 measurement (section 4).
2. **State store** — SQLite vs append-only log (Phase 2).
3. **Concurrency-gate policy** — queue vs degrade threshold, and whether interactive cloud requests preempt queued batch (Phase 2).
4. **Authn/z between hosts** — mTLS vs tailnet-only trust (Phase 2/4).
5. **Whether "local" small models on rpi4 (qwen2.5:1.5b) should ever serve interactive requests** — likely no; embeddings only (Phase 2).

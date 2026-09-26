# Spec: Lattice Failure & Fallback (Phase 5)

**Status:** Draft
**Host:** RPi4

Lattice must be robust. The "Lattice" should not be a single point of failure, and it must strictly adhere to privacy constraints even during outages.

## 1. Health Monitoring

The Control plane must track the health of the Mac Gateway.

- **Active Probe**: Control plane pings `/health` on the Gateway every 10s.
- **Passive Probe**: Every failed request to the Gateway increments a failure counter.
- **Circuit Breaker** *(not yet implemented)*: planned — if failure rate > 20% over 1 minute, mark the Gateway UNHEALTHY. The current implementation relies on the active health probe only.

## 2. Fallback Logic

**Recorded drift (2026-09-26): there is no fallback.** The table below is the
design intent; the code does not implement it.

When the Mac Gateway is UNHEALTHY:

| privacy | routing decision | action |
|---|---|---|
| `LOCAL_ONLY` | local | **Fail Closed** $\rightarrow$ return `503 Service Unavailable` |
| `LOCAL_PREFERRED` | local | **Fallback to Cloud** *(not implemented)* |
| `CLOUD_ALLOWED` | local | **Fallback to Cloud** *(not implemented)* |

What the code does instead: a request whose model resolves local — every request
that is neither cloud-tagged nor `interactive` — returns `503 No healthy local
gateway found`. `LOCAL_ONLY` fails closed as designed; so, today, does `batch`
traffic, which the design intended to be the very case that *did* fall back.
Latency class and model tag are what move traffic to cloud; gateway health never
does. See [handbook §8](../handbook/architecture-handbook.md).

The Gateway can also signal its own pressure (`429 Too Many Requests`). The
Control plane **does not re-route on it**: it is not in the response path — it
hands the decision to the frontend, which proxies to the target directly — so a
target-side `429` is surfaced to the client and recorded in telemetry, and the
control plane never learns of it.

## 3. Implementation Plan

- **Control Plane Update**:
  - Add a `HealthManager` that probes the Gateway. — **Done** (`monitorHealth()`, a
    10-second active probe; the passive counter and circuit breaker of §1 are not).
  - Update routing logic to check health before assigning a target. — **Done.**
  - Implement the fallback table. — **Not done.** Nothing falls back (§2).
- **Gateway Update**:
  - Add a `/health` endpoint (checks Ollama and memory pressure). — **Done.**
- **Exit Test** *(as designed; the middle case does not hold today)*:
  - Kill the Gateway process.
  - Send a `LOCAL_PREFERRED` request $\rightarrow$ ~~should be routed to Cloud~~ —
    actually returns `503 No healthy local gateway found`, because there is no
    fallback (§2).
  - Send a `LOCAL_ONLY` request $\rightarrow$ should return `503`. — **Holds.**
  - Restart Gateway $\rightarrow$ route should return to Local. — **Holds.**

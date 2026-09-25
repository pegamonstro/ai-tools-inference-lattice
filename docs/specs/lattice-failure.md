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

When the Mac Gateway is UNHEALTHY:

| privacy | routing decision | action |
|---|---|---|
| `LOCAL_ONLY` | local | **Fail Closed** $\rightarrow$ return `503 Service Unavailable` |
| `LOCAL_PREFERRED` | local | **Fallback to Cloud** $\rightarrow$ route to rpi4 Cloud |
| `CLOUD_ALLOWED` | local | **Fallback to Cloud** $\rightarrow$ route to rpi4 Cloud |

The Gateway itself can also signal pressure (e.g., `429 Too Many Requests`). The Control plane should treat a `429` as a temporary local failure and trigger the fallback logic.

## 3. Implementation Plan

- **Control Plane Update**:
  - Add a `HealthManager` that probes the Gateway.
  - Update routing logic to check health before assigning a target.
  - Implement the fallback table.
- **Gateway Update**:
  - Add a `/health` endpoint (checks Ollama and memory pressure).
- **Exit Test**:
  - Kill the Gateway process.
  - Send a `LOCAL_PREFERRED` request $\rightarrow$ should be routed to Cloud.
  - Send a `LOCAL_ONLY` request $\rightarrow$ should return `503`.
  - Restart Gateway $\rightarrow$ route should return to Local.

# Spec: Lattice Observability & Hardening (Phase 7)

**Status:** Draft
**Host:** RPi4

Lattice must be observable to verify that the routing policy is working as intended and to identify bottlenecks in the Mac Gateway.

## 1. Telemetry Requirements

Every routed request must generate a telemetry event:

- **Trace ID**: `request_id` from the `inference.v1` protocol.
- **Timing**:
  - `decision_time`: Time taken by the Control plane to route.
  - `execution_time`: Time taken by the target (Gateway/Cloud) to respond.
  - `total_time`: End-to-end wall-clock.
- **Routing Outcome**:
  - `requested_latency_class` (interactive/batch).
  - `final_target` (local/cloud).
  - `privacy_level` (LOCAL_ONLY / etc).
- **Resource Usage**:
  - `tokens_used` (prompt/completion).
  - `target_model` (e.g., `granite4:3b`).

### 1.1 The requirement holds for failures too

A line is written on **every exit path**, not only on the one that routed. Each
layer records the reason in an `error` field beside the model name — the
requested name until a decision resolves it — because a refusal is the event the
routing policy exists to produce: a `409` that appeared only in the client's
error would leave the policy unobservable at exactly the moment it acted.

Two consequences are part of the contract:

- **A layer that never ran writes nothing.** A fail-closed `503` is control's
  line alone; a target's own failure is the target's. That absence is the only
  legitimate one, and it is how a reader tells "did not run" from "failed".
- **An abort must not take the line with it.** Both handlers write from a
  `defer`, so a proxy abort unwinding the request — the realistic case being a
  client that disconnects mid-response — still leaves the line.

## 2. Observability Stack

To keep the system "deterministic" and avoid "speculative infrastructure" (Rule 5), we avoid Prometheus/Grafana for now.

- **Local Logging**: both planes write JSON lines, on paths that are
  environment-driven like every other address: control to
  `/var/log/lattice/telemetry-control.jsonl` (`LATTICE_CONTROL_TELEMETRY`), the
  frontend to `/var/log/lattice/telemetry-frontend.jsonl`
  (`LATTICE_FRONTEND_TELEMETRY`).
- **Summary Tool**: A small Go utility `lattice-stats` that reads the log and prints a summary:
  - Average latency per target.
  - Total token spend (Cloud).
  - Routing distribution (% cloud vs % local).

## 3. Hardening

- **Timeout Enforcement**: Set strict timeouts on all proxies (e.g., 30s for interactive, 1 hour for batch).
- **Request Validation**: Strict schema validation for `inference.v1` at the Frontend.
- **Resource Isolation**: Ensure the Control plane's CPU/MEM usage is capped so it doesn't interfere with the RPi4's other duties.

## 4. Implementation Plan

1. **Telemetry middleware**: Add to the Frontend and Control plane to log requests.
2. **Log rotation**: Basic setup for `/var/log/lattice/`.
3. **Lattice-stats tool**: Implement the analysis utility.
4. **Hardening**: Add timeouts and validation.

## 5. Exit Test (Phase 7)

- Generate 100 routed requests (mixed batch/interactive).
- Run `lattice-stats` and verify the routing distribution matches the policy.
- Verify that long-running requests are timed out correctly.

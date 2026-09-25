# Spec: Lattice Observability & Hardening (Phase 7)

**Status:** Draft
**Host:** RPi4 (`user@rpi4`)

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

## 2. Observability Stack

To keep the system "deterministic" and avoid "speculative infrastructure" (Rule 5), we avoid Prometheus/Grafana for now.

- **Local Logging**: All telemetry events written as JSON lines to `/var/log/lattice/telemetry.jsonl`.
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

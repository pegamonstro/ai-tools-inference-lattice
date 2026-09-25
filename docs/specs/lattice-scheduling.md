# Spec: Advanced Scheduling (Phase 9)

**Status:** Draft
**Host:** RPi4 (`user@rpi4`)

Lattice must ensure that high-priority interactive requests are not blocked by large batch workloads. This requires a priority-aware scheduler in the Control plane.

## 1. Priority Classes

| Class | Latency Class | Priority | Behavior |
|---|---|---|---|
| **Critical** | interactive | High | Jump to front of queue; preempt batch workers if possible. |
| **Standard** | batch | Low | Processed in background; subject to queueing. |

## 2. Scheduling Logic

The Control plane replaces the simple semaphore with a **Priority Queue**:

1. **Incoming Request**:
   - If `latency_class == interactive` $\rightarrow$ Push to `high_priority_queue`.
   - If `latency_class == batch` $\rightarrow$ Push to `low_priority_queue`.
2. **Dispatcher**:
   - Always check `high_priority_queue` first.
   - If a cloud slot is available, dispatch the oldest high-priority request.
   - If no high-priority requests exist, dispatch the oldest low-priority request.
3. **Cloud Spill**:
   - If `high_priority_queue` grows too large, the system may decide to spill some interactive requests to Local (Mac) if acceptable, or reject with `429`.

## 3. Implementation Plan

- **Update Control Plane**:
  - Replace `cloudSemaphore` with a dispatcher loop and two channels (`high_priority`, `low_priority`).
  - Implement the priority dispatch logic.
- **Exit Test**:
  - Send 10 batch requests, then 1 interactive request.
  - Verify that the interactive request is processed before the remaining batch requests.

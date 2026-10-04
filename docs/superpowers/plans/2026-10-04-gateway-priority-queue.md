# Gateway Priority Queue + Metrics — Implementation Plan (Phase 1)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace the gateway's FIFO slot semaphore with a weighted priority queue (with aging), bound the queue, and expose live Prometheus-style metrics including queue depth.

**Architecture:** The gateway (`cmd/lattice-gateway`) already serialises concurrent inference through `slotSemaphore`, a `sync.Cond`-based counting semaphore whose `acquire` blocks FIFO until a slot frees. Phase 1 swaps that for a `prioritySemaphore` that orders waiters by effective priority (weighted tier + aging factor), adds a bounded queue that rejects `429` when full, and feeds a new `/metrics` endpoint.

**Tech Stack:** Go 1.27 (stdlib only — no external deps), `sync.Cond`, `container/heap`-free O(n) waiter scan (queue depth is tiny in this homelab).

**Spec:** `docs/inference-queue-and-prioritisation.md` (Phase 1, decisions D2 + D3 + D6).

## Global Constraints

- **Stdlib only.** `go.mod` has no `require` block; do not add dependencies.
- **Priority tiers** are `interactive=0`, `default=1`, `batch=2` (lower = higher precedence).
- **Aging** uses an effective priority `eff = priority - agingWeight * secondsWaited`, so a long-waiting low-priority request eventually outranks a fresh high-priority one. `agingWeight` default `1.0`, env `LATTICE_GATEWAY_AGING_WEIGHT`.
- **Bounded queue:** max waiters `100` (env `LATTICE_GATEWAY_MAX_QUEUE`); reject `429` when full.
- **Do NOT remove the memory-pressure `429`** in this phase. Memory pressure keeps driving the slot *limit* via `computeSlots`; the per-request `CanAccommodate` backstop stays as-is. (The "queue instead of reject on memory pressure" change is deferred to Phase 2 — it's a swap-safety tradeoff, not a queueing requirement.)
- **Metrics** are Prometheus text format on `/metrics`; keep the existing JSONL telemetry and `/telemetry` relay untouched.
- Preserve the existing `setLimit`/`limitValue`/`release` semantics and the context-cancellation contract (abandoned requests must not hold a slot).

---

### Task 1: Priority semaphore (weighted + aging + bounded queue)

**Files:**
- Modify: `cmd/lattice-gateway/main.go` (replace `slotSemaphore`, lines 328–397, and `acquireSlot`, lines 796–802)
- Test: `cmd/lattice-gateway/slots_test.go`

**Interfaces:**
- Produces: `prioritySemaphore` with methods `acquire(ctx, priority) (bool, error)`, `release()`, `setLimit(n)`, `limitValue()`, `waiting() int` (queue depth), and sentinel `errQueueFull`.
- Consumes (Task 2/3): `waiting()` for the queue-depth gauge; `acquire`'s `(bool, error)` at the four handler call sites.

- [ ] **Step 1: Write the failing test**

Add to `slots_test.go` (update existing call sites to the new `acquire(ctx, priority)` signature first — the four existing tests call `s.acquire(ctx)`; change them to `s.acquire(ctx, 1)` and assert `granted==true, err==nil`):

```go
func TestPrioritySemaphoreOrdering(t *testing.T) {
    s := newPrioritySemaphore(1, 0.0) // aging off for a deterministic ordering test
    s.acquire(context.Background(), 1) // hold the single slot

    done := make(chan int, 3)
    // Enqueue batch(2) first, then interactive(0): interactive must win the slot.
    for _, p := range []int{2, 0, 1} {
        go func(p int) { s.acquire(context.Background(), p); done <- p }(p)
    }

    // First release grants the interactive(0) waiter.
    s.release()
    select {
    case p := <-done:
        if p != 0 { t.Fatalf("first granted priority = %d, want 0", p) }
    case <-time.After(time.Second):
        t.Fatal("no waiter granted after release")
    }
}

func TestPrioritySemaphoreQueueFull(t *testing.T) {
    s := newPrioritySemaphore(1, 0.0)
    s.setMaxQueue(1)
    s.acquire(context.Background(), 1) // hold the slot
    granted, err := s.acquire(context.Background(), 1)
    if granted || err != errQueueFull {
        t.Fatalf("acquire = (%v, %v), want (false, errQueueFull)", granted, err)
    }
}

func TestPrioritySemaphoreWaiting(t *testing.T) {
    s := newPrioritySemaphore(1, 0.0)
    s.acquire(context.Background(), 1)
    go s.acquire(context.Background(), 1)
    time.Sleep(30 * time.Millisecond)
    if got := s.waiting(); got != 1 {
        t.Fatalf("waiting() = %d, want 1", got)
    }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/lattice-gateway/ -run 'TestPrioritySemaphore'`
Expected: FAIL (compile error — `prioritySemaphore`/`errQueueFull`/`setMaxQueue` undefined).

- [ ] **Step 3: Implement `prioritySemaphore`**

Replace `slotSemaphore` (main.go:328–397) with:

```go
var errQueueFull = fmt.Errorf("inference queue full")

type waiter struct {
    priority int
    enqueued time.Time
}

// prioritySemaphore is a dynamic counting semaphore that orders waiters by
// effective priority (tier minus aging), not arrival order.
type prioritySemaphore struct {
    mu      sync.Mutex
    cond    *sync.Cond
    used    int
    limit   int
    maxQ    int
    aging   float64 // priority points granted per second of waiting
    waiters []waiter
}

func newPrioritySemaphore(limit int, aging float64) *prioritySemaphore {
    s := &prioritySemaphore{limit: limit, maxQ: 100, aging: aging}
    s.cond = sync.NewCond(&s.mu)
    return s
}

func (s *prioritySemaphore) setLimit(n int) {
    s.mu.Lock(); s.limit = n; s.cond.Broadcast(); s.mu.Unlock()
}
func (s *prioritySemaphore) limitValue() int {
    s.mu.Lock(); defer s.mu.Unlock(); return s.limit
}
func (s *prioritySemaphore) setMaxQueue(n int) {
    s.mu.Lock(); s.maxQ = n; s.mu.Unlock()
}
func (s *prioritySemaphore) waiting() int {
    s.mu.Lock(); defer s.mu.Unlock(); return len(s.waiters)
}

// effPriority is the aging-adjusted precedence; lower wins.
func (s *prioritySemaphore) effPriority(w waiter) float64 {
    return float64(w.priority) - s.aging*time.Since(w.enqueued).Seconds()
}

func (s *prioritySemaphore) acquire(ctx context.Context, priority int) (bool, error) {
    s.mu.Lock()
    if len(s.waiters) >= s.maxQ {
        s.mu.Unlock()
        return false, errQueueFull
    }
    w := waiter{priority: priority, enqueued: time.Now()}
    s.waiters = append(s.waiters, w)
    for {
        if ctx.Err() != nil {
            s.removeWaiter(w)
            s.mu.Unlock()
            return false, nil
        }
        if s.used < s.limit && s.isHead(w) {
            s.removeWaiter(w)
            s.used++
            s.mu.Unlock()
            return true, nil
        }
        s.wait(ctx)
    }
}

// isHead reports whether w is the minimum-effective-priority waiter.
func (s *prioritySemaphore) isHead(w waiter) bool {
    for _, other := range s.waiters {
        if s.effPriority(other) < s.effPriority(w) {
            return false
        }
    }
    return true
}

func (s *prioritySemaphore) removeWaiter(w waiter) {
    for i, other := range s.waiters {
        if other == w {
            s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
            return
        }
    }
}

// wait blocks until a release/setLimit broadcast or the caller's context is
// cancelled, matching the old semaphore's cancellation contract.
func (s *prioritySemaphore) wait(ctx context.Context) {
    done := ctx.Done()
    if done == nil {
        s.cond.Wait()
        return
    }
    stopped := make(chan struct{})
    go func() {
        select {
        case <-done:
            s.cond.Broadcast()
        case <-stopped:
        }
    }()
    s.cond.Wait()
    close(stopped)
}

func (s *prioritySemaphore) release() {
    s.mu.Lock(); s.used--; s.cond.Broadcast(); s.mu.Unlock()
}
```

Replace `inferenceSlots = newSlotSemaphore(1)` (main.go:868) with `inferenceSlots = newPrioritySemaphore(1, agingWeight())`, adding:

```go
func agingWeight() float64 {
    if v := latticeconfig.Env("LATTICE_GATEWAY_AGING_WEIGHT", ""); v != "" {
        if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
            return f
        }
    }
    return 1.0
}
```

And `maxQueueDepth` in `main()` from `LATTICE_GATEWAY_MAX_QUEUE` (default 100), applied via `inferenceSlots.setMaxQueue(n)`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./cmd/lattice-gateway/ -run 'TestPrioritySemaphore|TestSlotSemaphore'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/lattice-gateway/main.go cmd/lattice-gateway/slots_test.go
git commit -m "feat(gateway): weighted+aging priority semaphore with bounded queue"
```

---

### Task 2: Metrics registry + /metrics endpoint

**Files:**
- Create: `cmd/lattice-gateway/metrics.go`
- Modify: `cmd/lattice-gateway/main.go` (call `metrics.observe` from `logTelemetry`, register `/metrics` in `newRouter`)
- Test: `cmd/lattice-gateway/metrics_test.go`

**Interfaces:**
- Produces: package-level `metrics` value with `observe(te Telemetry)` and `handleMetrics(w, r)`; reads `inferenceSlots.waiting()`/`limitValue()`/`used()`.
- Consumes: `logTelemetry` (single choke point for request completion).

- [ ] **Step 1: Write the failing test**

```go
func TestMetricsEndpoint(t *testing.T) {
    metrics.observe(Telemetry{Model: "flux-dev", Elapsed: 1.5})
    rr := httptest.NewRecorder()
    handleMetrics(rr, httptest.NewRequest("GET", "/metrics", nil))
    body := rr.Body.String()
    for _, want := range []string{
        "lattice_gateway_requests_total",
        "lattice_gateway_request_duration_seconds",
        "lattice_gateway_queue_depth",
        "lattice_gateway_slots_limit",
    } {
        if !strings.Contains(body, want) {
            t.Fatalf("/metrics missing %q", want)
        }
    }
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./cmd/lattice-gateway/ -run TestMetricsEndpoint` → FAIL (undefined `metrics`).

- [ ] **Step 3: Implement `metrics.go`**

```go
package main

import (
    "fmt"
    "net/http"
    "sort"
    "sync"
)

// metrics holds process-wide counters/gauges, updated from logTelemetry and
// served as Prometheus text format at /metrics.
var metrics = newMetrics()

type metricsStore struct {
    mu              sync.Mutex
    requestsTotal   map[string]int64 // keyed by model
    errorsTotal     map[string]int64 // keyed by error reason
    durationSum     float64
    durationCount   int64
}

func newMetrics() *metricsStore {
    return &metricsStore{
        requestsTotal: make(map[string]int64),
        errorsTotal:   make(map[string]int64),
    }
}

func (m *metricsStore) observe(te Telemetry) {
    m.mu.Lock()
    m.requestsTotal[te.Model]++
    m.durationSum += te.Elapsed
    m.durationCount++
    if te.Error != "" {
        m.errorsTotal[te.Error]++
    }
    m.mu.Unlock()
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "text/plain; version=0.0.4")
    m := metrics
    m.mu.Lock()
    defer m.mu.Unlock()

    fmt.Fprintf(w, "# TYPE lattice_gateway_requests_total counter\n")
    for _, model := range sortedKeys(m.requestsTotal) {
        fmt.Fprintf(w, "lattice_gateway_requests_total{model=%q} %d\n", model, m.requestsTotal[model])
    }
    fmt.Fprintf(w, "# TYPE lattice_gateway_request_duration_seconds summary\n")
    if m.durationCount > 0 {
        fmt.Fprintf(w, "lattice_gateway_request_duration_seconds_sum %f\n", m.durationSum)
        fmt.Fprintf(w, "lattice_gateway_request_duration_seconds_count %d\n", m.durationCount)
    }
    fmt.Fprintf(w, "# TYPE lattice_gateway_queue_depth gauge\n")
    fmt.Fprintf(w, "lattice_gateway_queue_depth %d\n", inferenceSlots.waiting())
    fmt.Fprintf(w, "# TYPE lattice_gateway_slots_limit gauge\n")
    fmt.Fprintf(w, "lattice_gateway_slots_limit %d\n", inferenceSlots.limitValue())
}

func sortedKeys(m map[string]int64) []string {
    ks := make([]string, 0, len(m))
    for k := range m { ks = append(ks, k) }
    sort.Strings(ks)
    return ks
}
```

Wire `metrics.observe(te)` as the first line of `logTelemetry` (main.go:187), and add `mux.HandleFunc("/metrics", handleMetrics)` to `newRouter` (main.go:1384).

- [ ] **Step 4: Run to verify it passes** — `go test ./cmd/lattice-gateway/ -run TestMetricsEndpoint` → PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/lattice-gateway/metrics.go cmd/lattice-gateway/metrics_test.go cmd/lattice-gateway/main.go
git commit -m "feat(gateway): Prometheus /metrics endpoint with queue depth"
```

---

### Task 3: Wire priority into handlers + queue-full rejection

**Files:**
- Modify: `cmd/lattice-gateway/main.go` (four handlers + `acquireSlot` signature)
- Test: `cmd/lattice-gateway/main_test.go` (or existing handler tests)

**Interfaces:**
- Consumes: `prioritySemaphore.acquire(ctx, priority) (bool, error)` from Task 1.
- Produces: `priorityFor(latencyClass string) int` mapping `interactive→0, batch→2, else→1`; `headerPriority(r) int` reading `X-Priority` (fallback 1).

- [ ] **Step 1: Write the failing test**

```go
func TestPriorityFor(t *testing.T) {
    cases := map[string]int{"interactive": 0, "default": 1, "batch": 2, "": 1, "bogus": 1}
    for in, want := range cases {
        if got := priorityFor(in); got != want {
            t.Fatalf("priorityFor(%q) = %d, want %d", in, got, want)
        }
    }
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./cmd/lattice-gateway/ -run TestPriorityFor` → FAIL (undefined).

- [ ] **Step 3: Implement**

Add:

```go
func priorityFor(latencyClass string) int {
    switch latencyClass {
    case "interactive":
        return 0
    case "batch":
        return 2
    default:
        return 1
    }
}

func headerPriority(r *http.Request) int {
    return priorityFor(r.Header.Get("X-Priority"))
}
```

Change `acquireSlot` (main.go:796–802) to:

```go
func acquireSlot(ctx context.Context, priority int) (bool, error) {
    return inferenceSlots.acquire(ctx, priority)
}
```

Update the four handlers to pass a priority and handle `errQueueFull`:
- `handleInference`: `priorityFor(req.Routing.LatencyClass)` at both the stream path (line 944) and the unary path (line 973).
- `handleEmbeddings`/`handleImage`/`handleSpeech`: `headerPriority(r)`.

Each call site becomes:

```go
granted, err := acquireSlot(r.Context(), priority)
if err == errQueueFull {
    logTelemetry(Telemetry{RequestID: id, Model: m, Elapsed: time.Since(t0).Seconds(), Error: "queue_full"})
    http.Error(w, "Inference queue full; retry later", http.StatusTooManyRequests)
    return
}
if !granted {
    logQueuedCancel(req, ctxWindow, t0) // or the equivalent inline telemetry
    return
}
```

- [ ] **Step 4: Run to verify it passes** — `go test ./cmd/lattice-gateway/` (full package) → PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/lattice-gateway/main.go cmd/lattice-gateway/main_test.go
git commit -m "feat(gateway): route request priority into the queue and reject when full"
```

---

## Self-review notes

- The four handlers duplicate the same acquire→telemetry→release skeleton; Task 3 touches all four call sites identically — keep the diff mechanical and consistent (batch them, don't refactor).
- `handleInference`'s streaming path (line 942–971) and unary path (line 973–999) both call `acquireSlot`; update both.
- The existing `TestSlotSemaphore*` tests in `slots_test.go` must be renamed/updated to `prioritySemaphore` in Task 1 (the old type is deleted).
- `wait()`'s per-waiter broadcast goroutine is identical in spirit to the old `acquire`'s cancellation goroutine; keep it — abandoning a waiter must wake the cond so the head can advance.

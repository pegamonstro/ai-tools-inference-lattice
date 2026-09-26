# Lattice Locality and Embeddings Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `locality` (`local` | `cloud` | `unknown`) a reported field on every routing decision and every telemetry line, and serve `/v1/embeddings` through the frontend and the gateway.

**Architecture:** Both changes are *exposure*, not policy. Control already knows which registry a target came from (`gateways` → local, `providers` → cloud); the frontend already knows the decision; Ollama already serves `/v1/embeddings` natively. So the field is derived by one pure function beside the registries, the frontend's literal name match becomes a read of that field, the gateway forwards embeddings to Ollama's own OpenAI-compatible endpoint without translating, and the embedding request shares the gateway's single inference slot.

**Tech Stack:** Go 1.27 stdlib only. No new dependencies.

**Spec:** [docs/specs/lattice-locality-and-embeddings.md](../specs/lattice-locality-and-embeddings.md)

## Global Constraints

Copied verbatim from the spec; every task's requirements implicitly include them.

- Go standard library only. No new dependencies; no modules beyond the existing `go.mod`.
- No real usernames, hostnames, or IP addresses may appear in any tracked file. Use `<placeholder>`.
- No speculative infrastructure (anti-drift rule 5): no embedding cache, batch queue, second process, or new telemetry store.
- Local concurrency is 1. The embedding request takes the gateway's one inference slot; it must never run beside a chat turn.
- `locality` values are exactly `local`, `cloud`, `unknown` — the **same strings** already in a registry entry's `Capabilities`, so no translation table exists to drift. `locality` sits **beside** `target`, never instead of it.
- Consumers never derive locality from a target name. Adding a value to the enum must be non-breaking.
- Public behaviour must keep working: `/v1/chat/completions`, `/v1/models`, and `/health` are unchanged for any client.
- No change to routing policy. `LOCAL_ONLY` and the model-tag cloud rule are untouched.
- `lattice-stats` is **out of scope**: its rewrite is Task 5 of the build-orchestration plan. This plan only makes the field available to it.

**Deploy order is load-bearing** (Task 9): control first, then frontend, then gateway. A frontend running ahead of control reads an empty `locality` and withholds the routing envelope from a local target. That fails *safe* — routing never leaks to cloud — but the gateway's telemetry line loses its key until control catches up.

---

### Task 1: Correct the spec's claims about `lattice-stats`

The spec this plan argues from makes three claims about `cmd/lattice-stats/main.go` that the file does not support: it has **no** literal-name match and **no** cloud/local split. Its `Telemetry` struct parses only `request_id`, `total_time_s`, `execution_time_s`, and `target`, and it prints a per-target routing distribution. Correcting the spec first, so no later task is written against a false premise.

**Files:**
- Modify: `docs/specs/lattice-locality-and-embeddings.md`

**Interfaces:**
- Consumes: nothing.
- Produces: nothing executable. Every later task quotes this document.

- [ ] **Step 1: Verify the claim before acting on it**

Run: `grep -n 'mac-gateway\|ollama-cloud-secondary\|cloud\|local' cmd/lattice-stats/main.go`

Expected: no output involving the two literal target names, and no comparison against `"cloud"` or `"local"`. If either appears, this task is unnecessary — stop and re-read §3.2/§3.6 before editing.

- [ ] **Step 2: Narrow the Scope line**

Replace line 4:

```markdown
**Scope:** `lattice-control`, `lattice-frontend`, `lattice-gateway`
```

- [ ] **Step 3: Fix the §1 claim**

Replace the third bullet of §1:

```markdown
- **Every new consumer pays the name-matching tax again.** The frontend already
  carries it (§2), and a stats rewrite would need a second copy of it. The
  registry the rule comes from lives in control, so no other layer can derive
  the answer without either reaching for that registry or hardcoding the names.
```

- [ ] **Step 4: Reframe the §3.2 consumer row**

Replace the `lattice-stats` row of the §3.2 table (the last row):

```markdown
| `lattice-stats` *(not in this plan)* | — | once rewritten, reads `locality` for §2's split |
```

- [ ] **Step 5: Delete the §3.6 row that names a literal that does not exist**

§3.6's table has two rows and the second is wrong. Replace the whole table:

```markdown
| literal | replaced by |
|---|---|
| `decision.Target == "mac-gateway"` in the frontend | `decision.Locality == "local"` |
```

- [ ] **Step 6: Check the rest of the document for the same claim**

Run: `grep -n 'lattice-stats' docs/specs/lattice-locality-and-embeddings.md`

Expected: hits only in the Step 4 row and in §9.5. Any hit asserting that `lattice-stats` *currently* matches names or *currently* splits cloud from local is a further instance of the same error — fix it the same way.

- [ ] **Step 7: Commit**

```bash
git add docs/specs/lattice-locality-and-embeddings.md
git commit -m "Correct what the locality spec says about lattice-stats"
```

---

### Task 2: Control — `locality` as a derived attribute

Locality is derived from the registry the target was chosen from, by a pure function, so it cannot be forgotten on a branch and can be tested without the dispatcher.

`★ Insight ─────────────────────────────────────`
The cloud branch of `handleRoute` blocks on `<-respCh`, and only `main()` starts the goroutine that reads it — so a unit test asserting a successful **cloud** decision's locality would hang forever. Deriving locality in the *deferred* telemetry write, from `tele.Target` rather than from a per-branch variable, sidesteps this entirely: the refusal paths pick no target and get `unknown` for free, and there is one call site rather than four.

The dispatcher at `cmd/lattice-control/main.go:437` echoes the *same* `Decision` struct through `ResponseCh`, so a field set before dispatch survives the round trip and needs no separate handling.
`─────────────────────────────────────────────────`

**Files:**
- Modify: `cmd/lattice-control/main.go` (struct `Decision` at ~34, struct `Telemetry` at ~40, `handleRoute` at ~558)
- Test: `cmd/lattice-control/route_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func localityFor(targetID string) string`, returning `"local"` for a `gateways` key, `"cloud"` for a `providers` key, `"unknown"` otherwise (including `""`). The `Decision` JSON gains `locality`; the control telemetry schema gains `locality`.

- [ ] **Step 1: Write the failing rule test**

Append to `cmd/lattice-control/route_test.go`:

```go
// Locality is a property of the registry a target was chosen from, not of its
// name. Keying it on names is what every consumer had to re-derive, and it
// misclassifies silently the moment a target is added or renamed.
func TestLocalityFor(t *testing.T) {
	cases := []struct {
		name   string
		target string
		want   string
	}{
		{"a gateway is local", "mac-gateway", "local"},
		{"a provider is cloud", "ollama-cloud-primary", "cloud"},
		{"the second provider is cloud too", "ollama-cloud-secondary", "cloud"},
		// A refusal picks no target, and a refusal is an event the cloud/local
		// split must still be able to account for.
		{"no target is unknown", "", "unknown"},
		{"an unrecognised target is unknown", "somewhere-else", "unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := localityFor(tc.target); got != tc.want {
				t.Errorf("localityFor(%q) = %q, want %q", tc.target, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cmd/lattice-control/ -run TestLocalityFor -v`
Expected: FAIL — `undefined: localityFor`.

- [ ] **Step 3: Write the rule**

In `cmd/lattice-control/main.go`, immediately after `init()`:

```go
// localityFor reports which registry a target was chosen from. It is derived
// rather than tracked per branch so that no exit path can forget it, and so that
// a new target is classified by where it was registered rather than by its name.
//
// The values are the same strings the registries already use in Capabilities, so
// there is one vocabulary and no translation table to drift.
func localityFor(targetID string) string {
	if _, ok := gateways[targetID]; ok {
		return "local"
	}
	if _, ok := providers[targetID]; ok {
		return "cloud"
	}
	return "unknown"
}
```

- [ ] **Step 4: Run the rule test to verify it passes**

Run: `go test ./cmd/lattice-control/ -run TestLocalityFor -v`
Expected: PASS, 5 subtests.

- [ ] **Step 5: Add the field to both structs**

In `cmd/lattice-control/main.go`, replace the `Decision` struct:

```go
type Decision struct {
	Target    string `json:"target"`
	Endpoint  string `json:"endpoint"`
	ModelName string `json:"model_name"`
	// Locality sits beside Target, never instead of it: Target stays the source
	// of truth and the key the per-target views are built on.
	Locality string `json:"locality"`
}
```

and the `Telemetry` struct:

```go
type Telemetry struct {
	RequestID    string  `json:"request_id"`
	DecisionTime float64 `json:"decision_time_s"`
	Target       string  `json:"target"`
	Model        string  `json:"model"`
	Privacy      string  `json:"privacy"`
	LatencyClass string  `json:"latency_class"`
	Locality     string  `json:"locality"`
	Error        string  `json:"error,omitempty"`
}
```

Neither field is `omitempty`. A line with no locality is a line the split cannot count, and `unknown` is a countable answer where an absent key is not.

- [ ] **Step 6: Emit it on the decision**

In `handleRoute`, replace the decision literal (currently at ~661):

```go
	decision := Decision{
		Target:    targetID,
		Endpoint:  endpoint,
		ModelName: modelName,
		Locality:  localityFor(targetID),
	}
```

- [ ] **Step 7: Emit it on the telemetry**

In `handleRoute`'s deferred writer (currently at ~568), add one line so the write order is `Target` then `Locality`:

```go
	defer func() {
		tele.RequestID = req.Routing.RequestID
		tele.DecisionTime = time.Since(t0).Seconds()
		tele.Privacy = req.Routing.Privacy
		tele.LatencyClass = req.Routing.LatencyClass
		// Derived from the target the body settled on, so every exit path is
		// covered by construction: the refusal paths never set a target, and
		// localityFor("") is "unknown".
		tele.Locality = localityFor(tele.Target)
		logTelemetry(tele)
	}()
```

- [ ] **Step 8: Assert the refusal and fail-closed paths report `unknown`**

In `TestHandleRouteRecordsARefusal`, after the existing `got.Target` assertion:

```go
	if got.Locality != "unknown" {
		t.Errorf("locality = %q, want unknown — a refusal chose no target", got.Locality)
	}
```

In `TestHandleRouteRecordsAFailClosedDecision`, after the existing `got.Model` assertion:

```go
	if got.Locality != "unknown" {
		t.Errorf("locality = %q, want unknown — no target was reachable", got.Locality)
	}
```

- [ ] **Step 9: Write the local-path test**

The local path is the only one exercisable without the dispatcher. Append to `cmd/lattice-control/route_test.go`:

```go
// The local path is asserted end to end — decision and telemetry — because it is
// the one path that completes without the response channel a test cannot start.
func TestHandleRouteReportsLocalLocality(t *testing.T) {
	old := telemetryPath
	telemetryPath = t.TempDir() + "/telemetry-control.jsonl"
	defer func() { telemetryPath = old }()

	healthMutex.Lock()
	oldHealthy := gatewayHealthy
	gatewayHealthy = map[string]bool{"mac-gateway": true}
	healthMutex.Unlock()
	defer func() {
		healthMutex.Lock()
		gatewayHealthy = oldHealthy
		healthMutex.Unlock()
	}()

	body := `{"model":"granite4:3b","messages":[],"routing":{"privacy":"LOCAL_ONLY","request_id":"req-local"}}`
	rec := httptest.NewRecorder()
	handleRoute(rec, httptest.NewRequest("POST", "/route", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var decision Decision
	if err := json.Unmarshal(rec.Body.Bytes(), &decision); err != nil {
		t.Fatalf("response is not a decision: %v (%s)", err, rec.Body.String())
	}
	if decision.Locality != "local" {
		t.Errorf("decision locality = %q, want local", decision.Locality)
	}
	if decision.Target != "mac-gateway" {
		t.Errorf("target = %q, want the healthy gateway", decision.Target)
	}

	logged, err := os.ReadFile(telemetryPath)
	if err != nil {
		t.Fatalf("no telemetry line: %v", err)
	}
	var got Telemetry
	if err := json.Unmarshal(bytes.TrimSpace(logged), &got); err != nil {
		t.Fatalf("telemetry is not one JSON line: %v (%s)", err, logged)
	}
	if got.Locality != "local" {
		t.Errorf("telemetry locality = %q, want local", got.Locality)
	}
}
```

- [ ] **Step 10: Run the whole package**

Run: `go test ./cmd/lattice-control/ -v`
Expected: PASS, including the two amended tests and the new one.

- [ ] **Step 11: Commit**

```bash
git add cmd/lattice-control/main.go cmd/lattice-control/route_test.go
git commit -m "Report locality on every control decision and telemetry line"
```

---

### Task 3: Frontend — read locality instead of a target name

The frontend's `buildProxyBody` decides whether to inject the routing envelope by matching the literal `"mac-gateway"`. It must read the field instead.

`★ Insight ─────────────────────────────────────`
`TestBuildProxyBodyPreservesClientFields` hand-builds a `Decision` with no `Locality` and asserts an envelope *is* present. Changing the production check without updating that test would break the suite in a way that looks like a test bug — the exact near-miss that makes "update the test" a plan step rather than an afterthought.

`TestHandleChatCorrelatesAllThreePlanes` has the same hazard one level up: its control stub returns `Decision{Target: "mac-gateway", ...}` with no locality, so the proxied request would lose its envelope and the target-side `sawTargetID` assertion would fail. Its stub needs `Locality: "local"` too.
`─────────────────────────────────────────────────`

**Files:**
- Modify: `cmd/lattice-frontend/main.go` (structs `Decision` at ~34 and `Telemetry` at ~73, `buildProxyBody` at ~47, `handleChat` at ~164)
- Test: `cmd/lattice-frontend/handler_test.go`

**Interfaces:**
- Consumes: control's `Decision.locality` from Task 2.
- Produces: `buildProxyBody(raw []byte, decision Decision, requestID string, providerParams map[string]interface{}) ([]byte, error)` — **signature unchanged** in this task. The frontend `Decision` and `Telemetry` structs gain `Locality string \`json:"locality"\``.

- [ ] **Step 1: Write the failing regression test**

Append to `cmd/lattice-frontend/handler_test.go`:

```go
// The envelope is injected because the target is local, not because its name is
// mac-gateway. Keyed on the name, a second gateway — or a renamed one — would
// silently stop receiving the envelope, and the loss is invisible: the gateway
// still answers, it just answers an unkeyed request.
func TestBuildProxyBodyKeysTheEnvelopeOnLocality(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	t.Run("a local target is given an envelope whatever it is called", func(t *testing.T) {
		out, err := buildProxyBody(raw,
			Decision{Target: "a-second-gateway", Locality: "local", ModelName: "x"}, "rid-local", nil)
		if err != nil {
			t.Fatalf("buildProxyBody: %v", err)
		}
		var got map[string]interface{}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("output is not JSON: %v", err)
		}
		routing, ok := got["routing"].(map[string]interface{})
		if !ok {
			t.Fatalf("a local target got no routing envelope: %v", got["routing"])
		}
		if routing["request_id"] != "rid-local" {
			t.Errorf("routing.request_id = %v, want rid-local", routing["request_id"])
		}
	})

	t.Run("the old literal no longer buys an envelope", func(t *testing.T) {
		out, err := buildProxyBody(raw,
			Decision{Target: "mac-gateway", Locality: "cloud", ModelName: "x"}, "rid-cloud", nil)
		if err != nil {
			t.Fatalf("buildProxyBody: %v", err)
		}
		var got map[string]interface{}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("output is not JSON: %v", err)
		}
		if _, ok := got["routing"]; ok {
			t.Errorf("the name alone still injects an envelope: %v", got["routing"])
		}
	})
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cmd/lattice-frontend/ -run TestBuildProxyBodyKeysTheEnvelopeOnLocality -v`
Expected: FAIL to compile — `unknown field Locality in struct literal`.

- [ ] **Step 3: Add the field to the frontend's `Decision`**

In `cmd/lattice-frontend/main.go`:

```go
type Decision struct {
	Target    string `json:"target"`
	Endpoint  string `json:"endpoint"`
	ModelName string `json:"model_name"`
	// Locality is the target's class as control derived it. The frontend reads it
	// rather than the target's name, so it holds no copy of the rule.
	Locality string `json:"locality"`
}
```

- [ ] **Step 4: Key the envelope on locality**

In `buildProxyBody`, replace the condition:

```go
	// The client's own routing envelope is never forwarded: on the local path it
	// is replaced by the one the frontend issues, and on the cloud path it must
	// not appear at all.
	delete(body, "routing")
	if decision.Locality == "local" {
		routing := map[string]interface{}{"request_id": requestID}
		if providerParams != nil {
			routing["provider_params"] = providerParams
		}
		body["routing"] = routing
	}
```

- [ ] **Step 5: Run the regression test to verify it passes**

Run: `go test ./cmd/lattice-frontend/ -run TestBuildProxyBodyKeysTheEnvelopeOnLocality -v`
Expected: PASS, 2 subtests.

- [ ] **Step 6: Update the test that hand-builds a local decision**

In `TestBuildProxyBodyPreservesClientFields`, replace the `decision` line:

```go
	decision := Decision{Target: "mac-gateway", Locality: "local", Endpoint: "http://example:8081", ModelName: "hermes3:8b"}
```

In `TestBuildProxyBodyStripsRoutingOnCloudPath`, replace the `decision` line so the cloud case is explicit rather than accidental:

```go
	decision := Decision{Target: "ollama-cloud-primary", Locality: "cloud", Endpoint: "http://example:11434", ModelName: "gemma4:31b-cloud"}
```

In `TestHandleChatCorrelatesAllThreePlanes`, replace the control stub's decision:

```go
		json.NewEncoder(w).Encode(Decision{Target: "mac-gateway", Locality: "local", Endpoint: target.URL, ModelName: "hermes3:8b"})
```

- [ ] **Step 7: Add `Locality` to the frontend's telemetry**

Replace the `Telemetry` struct:

```go
type Telemetry struct {
	RequestID     string  `json:"request_id"`
	TotalTime     float64 `json:"total_time_s"`
	ExecutionTime float64 `json:"execution_time_s"`
	Target        string  `json:"target"`
	Locality      string  `json:"locality"`
	Model         string  `json:"model,omitempty"`
	Error         string  `json:"error,omitempty"`
}
```

- [ ] **Step 8: Emit it, with a default for the paths that never got a decision**

In `handleChat`'s deferred writer, add the default beside the existing `RequestID` default:

```go
		if tele.RequestID == "" {
			tele.RequestID = resolveRequestID(Routing{})
		}
		// A refusal never reached a decision, and a control plane running behind
		// this binary reports no locality at all. Neither is a reason for the line
		// to leave the split: unknown is countable, absent is not.
		if tele.Locality == "" {
			tele.Locality = "unknown"
		}
```

and beside the existing `tele.Target` assignment, after the decision is decoded:

```go
	tele.Target = decision.Target
	tele.Locality = decision.Locality
	tele.Model = decision.ModelName
```

- [ ] **Step 9: Assert locality in the correlation test**

In `TestHandleChatCorrelatesAllThreePlanes`, replace the final telemetry block:

```go
	logged, err := os.ReadFile(telemetryPath)
	if err != nil || !strings.Contains(string(logged), header) {
		t.Errorf("frontend telemetry does not carry %q: %v %s", header, err, logged)
	}

	var line Telemetry
	if err := json.Unmarshal(bytes.TrimSpace(logged), &line); err != nil {
		t.Fatalf("frontend telemetry is not one JSON line: %v (%s)", err, logged)
	}
	if line.Locality != "local" {
		t.Errorf("locality = %q, want local — the split cannot be built from a name", line.Locality)
	}
```

- [ ] **Step 10: Run the whole package**

Run: `go test ./cmd/lattice-frontend/ -v`
Expected: PASS, all tests.

- [ ] **Step 11: Commit**

```bash
git add cmd/lattice-frontend/main.go cmd/lattice-frontend/handler_test.go
git commit -m "Read the target's locality instead of its name in the frontend"
```

---

### Task 4: Gateway — stamp locality inside `logTelemetry`

The gateway **is** the local executor, so its locality is a constant. It is stamped in `logTelemetry` rather than at each call site, because there are five call sites and a field one of them forgets is a field that silently disappears from the split.

**Files:**
- Modify: `cmd/lattice-gateway/main.go` (struct `Telemetry` at ~84, `logTelemetry` at ~126)
- Test: `cmd/lattice-gateway/telemetry_test.go` (create)

**Interfaces:**
- Consumes: nothing.
- Produces: the gateway telemetry schema gains `locality: "local"`. Since control relays these events verbatim (`Events []json.RawMessage`), the field reaches the Pi without any relay change.

- [ ] **Step 1: Write the failing test**

Create `cmd/lattice-gateway/telemetry_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// The gateway is the local executor, so its locality is a constant. It is set
// inside logTelemetry rather than at each call site: there are five call sites,
// and a field one of them forgets is a field that vanishes from the split
// without anyone noticing, because the line still looks complete.
func TestLogTelemetryStampsLocalLocality(t *testing.T) {
	path := t.TempDir() + "/telemetry-gateway.jsonl"
	t.Setenv("LATTICE_GATEWAY_TELEMETRY", path)

	logTelemetry(Telemetry{RequestID: "req-1", Model: "granite4:3b"})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("logTelemetry wrote no line: %v", err)
	}
	var got Telemetry
	if err := json.Unmarshal(bytes.TrimSpace(raw), &got); err != nil {
		t.Fatalf("telemetry is not one JSON line: %v (%s)", err, raw)
	}
	if got.Locality != "local" {
		t.Errorf("locality = %q, want local", got.Locality)
	}
	// The stamp is additive: the caller's own fields must survive it.
	if got.RequestID != "req-1" || got.Model != "granite4:3b" {
		t.Errorf("the caller's fields were clobbered: %+v", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cmd/lattice-gateway/ -run TestLogTelemetryStampsLocalLocality -v`
Expected: FAIL to compile — `unknown field Locality in struct literal`.

- [ ] **Step 3: Add the field**

Replace the gateway `Telemetry` struct:

```go
type Telemetry struct {
	RequestID        string  `json:"request_id"`
	Model            string  `json:"model"`
	ContextWindow    int     `json:"context_window"`
	Elapsed          float64 `json:"elapsed_s"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	// Locality is stamped in logTelemetry, not by callers: this process is the
	// local executor, so the value is constant, and setting it in one place makes
	// it true by construction at every call site.
	Locality         string  `json:"locality"`
	Error            string  `json:"error,omitempty"`
}
```

- [ ] **Step 4: Stamp it**

In `logTelemetry`, add the assignment as the first statement of the function body — after the signature, before the mutex:

```go
func logTelemetry(t Telemetry) {
	// Constant, and set here rather than by callers so no call site can forget
	// it. The gateway has no target field and does not gain one: its provenance
	// is this process, and locality is the one shared dimension the three streams
	// need.
	t.Locality = "local"

	telemetryMutex.Lock()
```

- [ ] **Step 5: Run it to verify it passes**

Run: `go test ./cmd/lattice-gateway/ -run TestLogTelemetryStampsLocalLocality -v`
Expected: PASS.

- [ ] **Step 6: Run the whole package**

Run: `go test ./cmd/lattice-gateway/ -v`
Expected: PASS, all tests — the stamp must not disturb any existing telemetry assertion.

- [ ] **Step 7: Commit**

```bash
git add cmd/lattice-gateway/main.go cmd/lattice-gateway/telemetry_test.go
git commit -m "Stamp locality on every gateway telemetry line"
```

---

### Task 5: Frontend — the `/v1/embeddings` route

An embeddings request shares the chat handler's *parse → control → proxy* flow. Two things differ: the upstream path, and the absence of a routing envelope.

`★ Insight ─────────────────────────────────────`
The frontend can unmarshal an embedding body into the existing `Request` struct without loss: control routes on `model`, `routing.privacy`, and `routing.latency_class`, and **never reads `messages`**. An `{"input": ...}` body therefore routes correctly with the fields control cares about populated and `Messages` left nil. And `buildProxyBody` operates on the *original* `bodyBytes`, so `input` survives untouched — the non-enumerating forward is what makes an unfamiliar body shape safe.

The gateway needs a correlation id on this route, and the envelope is not available (it is the chat translation contract). So the frontend sets `X-Request-Id` on the proxied request. That is the standard hop-tracing header, and on the chat path it is redundant but harmless — the gateway ignores request headers there.
`─────────────────────────────────────────────────`

**Files:**
- Modify: `cmd/lattice-frontend/main.go` (`buildProxyBody` at ~47, `handleChat` at ~164, `main` at ~259)
- Test: `cmd/lattice-frontend/handler_test.go`

**Interfaces:**
- Consumes: Task 3's `Decision`/`Telemetry` `Locality`, and control's routing of an untagged embedding model to the local capability (already correct — no routing change).
- Produces:
  - `func buildProxyBody(raw []byte, decision Decision, requestID string, providerParams map[string]interface{}, injectRouting bool) ([]byte, error)` — **5 parameters now**.
  - `func proxyInference(w http.ResponseWriter, r *http.Request, upstreamPath string, injectRouting bool)`
  - `func handleEmbeddings(w http.ResponseWriter, r *http.Request)`
  - `func newRouter() *http.ServeMux`
  - A request header `X-Request-Id` on the outbound proxied request (Task 6 reads it).

- [ ] **Step 1: Write the failing route test**

Append to `cmd/lattice-frontend/handler_test.go`:

```go
// An embedding is not a chat. The upstream path must be /v1/embeddings, the
// client's own body must arrive intact, and no routing envelope may be injected —
// that object is the chat translation layer's contract and an embedding has no
// such contract. The id rides a header instead, so the gateway's own line is
// still keyed and the request is correlatable across all three streams.
func TestHandleEmbeddingsForwardsWithoutARoutingEnvelope(t *testing.T) {
	var sawPath string
	var sawBody map[string]interface{}
	var sawHeader string

	var target *httptest.Server
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Decision{
			Target:    "mac-gateway",
			Locality:  "local",
			Endpoint:  target.URL,
			ModelName: "embeddinggemma:latest",
		})
	}))
	defer control.Close()

	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawHeader = r.Header.Get("X-Request-Id")
		json.NewDecoder(r.Body).Decode(&sawBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],"model":"embeddinggemma:latest"}`))
	}))
	defer target.Close()

	oldControl, oldTelemetry := controlURL, telemetryPath
	controlURL = control.URL
	telemetryPath = t.TempDir() + "/telemetry-frontend.jsonl"
	defer func() { controlURL, telemetryPath = oldControl, oldTelemetry }()

	body := `{"model":"embeddinggemma:latest","input":"a brief about the routing rule","encoding_format":"float"}`
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("X-Request-Id", "req-embed")
	rec := httptest.NewRecorder()
	handleEmbeddings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if sawPath != "/v1/embeddings" {
		t.Errorf("upstream path = %q, want /v1/embeddings", sawPath)
	}
	if sawBody["input"] != "a brief about the routing rule" {
		t.Errorf("input did not arrive intact: %v", sawBody["input"])
	}
	if sawBody["encoding_format"] != "float" {
		t.Errorf("encoding_format was dropped: %v", sawBody)
	}
	if _, ok := sawBody["routing"]; ok {
		t.Errorf("a routing envelope reached the embeddings path: %v", sawBody["routing"])
	}
	if sawHeader != "req-embed" {
		t.Errorf("X-Request-Id = %q, want the client's id — the gateway line would be unkeyed", sawHeader)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cmd/lattice-frontend/ -run TestHandleEmbeddingsForwardsWithoutARoutingEnvelope -v`
Expected: FAIL to compile — `undefined: handleEmbeddings`.

- [ ] **Step 3: Parameterise the routing envelope**

In `buildProxyBody`, change the signature and the condition. The locality condition stays: `injectRouting` answers "does this route have a routing contract?", the locality check answers "does this target speak it?", and they are different questions.

```go
// buildProxyBody forwards the client's own body with only two rewrites: the
// resolved model name, and — on a route that has one — the routing envelope
// (which belongs to the local translation layer only; a cloud endpoint speaks
// plain OpenAI and rejects it).
//
// It deliberately does not enumerate the fields it forwards. Enumerating is what
// broke this: the previous version rebuilt the body from model, messages and
// stream, so `tools` was dropped and an agent lost the ability to call anything.
func buildProxyBody(raw []byte, decision Decision, requestID string, providerParams map[string]interface{}, injectRouting bool) ([]byte, error) {
```

and:

```go
	// The client's own routing envelope is never forwarded: on the local path it
	// is replaced by the one the frontend issues, and on the cloud path it must
	// not appear at all. injectRouting says whether this route has a routing
	// contract at all; the locality check says whether this target speaks it.
	delete(body, "routing")
	if injectRouting && decision.Locality == "local" {
		routing := map[string]interface{}{"request_id": requestID}
		if providerParams != nil {
			routing["provider_params"] = providerParams
		}
		body["routing"] = routing
	}
```

- [ ] **Step 4: Rename `handleChat`'s body to `proxyInference`**

Rename the existing `handleChat` function to `proxyInference` and add the two parameters. Inside it, the only changes are the parameterised upstream path and the pass-through of `injectRouting`:

```go
func proxyInference(w http.ResponseWriter, r *http.Request, upstreamPath string, injectRouting bool) {
```

```go
	// 2. Rewrite request for Target
	finalBodyBytes, err := buildProxyBody(bodyBytes, decision, req.Routing.RequestID, req.Routing.ProviderParams, injectRouting)
```

```go
	// 3. Proxy to Target
	tExecStart = time.Now()
	targetURL, _ := url.Parse(decision.Endpoint)
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	r.URL.Path = upstreamPath
	r.Body = io.NopCloser(bytes.NewBuffer(finalBodyBytes))
	r.ContentLength = int64(len(finalBodyBytes))

	// Sent on both routes. Chat carries its id in the envelope; embeddings has no
	// envelope, so without this header its gateway line would be unkeyed and the
	// request would be correlatable in two planes out of three.
	r.Header.Set("X-Request-Id", req.Routing.RequestID)

	w.Header().Set("X-Request-Id", req.Routing.RequestID)

	proxy.ServeHTTP(w, r)
}
```

- [ ] **Step 5: Add the two route handlers**

Immediately above `proxyInference`:

```go
func handleChat(w http.ResponseWriter, r *http.Request) {
	proxyInference(w, r, "/v1/chat/completions", true)
}

// handleEmbeddings shares the chat flow — parse, ask control, proxy — and differs
// in exactly two ways: the upstream path, and the absence of a routing envelope.
// Embeddings are local-only by platform, because Ollama refuses them on its cloud
// passthrough, so control's existing resolution of an untagged embedding model to
// the local capability is already the whole routing story.
func handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	proxyInference(w, r, "/v1/embeddings", false)
}
```

- [ ] **Step 6: Add the router**

```go
// newRouter holds the route table so it can be asserted in a test: a route that
// exists in the source but is never registered is, from a client's side,
// indistinguishable from one that does not exist.
func newRouter() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChat)
	mux.HandleFunc("/v1/embeddings", handleEmbeddings)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/health", handleHealth)
	return mux
}
```

and replace `main`'s registration lines:

```go
func main() {
	addr := latticeconfig.Env("LATTICE_FRONTEND_ADDR", ":8080")
	fmt.Printf("Lattice Frontend listening on %s...\n", addr)
	log.Fatal(http.ListenAndServe(addr, newRouter()))
}
```

- [ ] **Step 7: Add the route-registration test**

```go
func TestRouterServesEveryClientFacingRoute(t *testing.T) {
	mux := newRouter()
	for _, path := range []string{"/v1/chat/completions", "/v1/embeddings", "/v1/models", "/health"} {
		if _, pattern := mux.Handler(httptest.NewRequest("POST", path, nil)); pattern != path {
			t.Errorf("%s is served as %q — an unregistered route is a 404 to the client", path, pattern)
		}
	}
}
```

- [ ] **Step 8: Update the four `buildProxyBody` call sites in the tests**

Each existing call gains a final `true` (they all exercise the chat route):

```go
	out, err := buildProxyBody(raw, decision, "rid-1", map[string]interface{}{"reasoning_effort": "low"}, true)
```
```go
	out, err := buildProxyBody(raw, decision, "rid-2", nil, true)
```
```go
	if _, err := buildProxyBody([]byte(`"just a string"`), Decision{ModelName: "x"}, "rid", nil, true); err == nil {
```
and both calls in `TestBuildProxyBodyKeysTheEnvelopeOnLocality`:

```go
		out, err := buildProxyBody(raw,
			Decision{Target: "a-second-gateway", Locality: "local", ModelName: "x"}, "rid-local", nil, true)
```
```go
		out, err := buildProxyBody(raw,
			Decision{Target: "mac-gateway", Locality: "cloud", ModelName: "x"}, "rid-cloud", nil, true)
```

- [ ] **Step 9: Run the whole package**

Run: `go test ./cmd/lattice-frontend/ -v`
Expected: PASS, including the new embeddings test and the registration test.

- [ ] **Step 10: Commit**

```bash
git add cmd/lattice-frontend/main.go cmd/lattice-frontend/handler_test.go
git commit -m "Serve /v1/embeddings through the frontend"
```

---

### Task 6: Gateway — the `/v1/embeddings` route

The gateway forwards to Ollama's own `/v1/embeddings`. Ollama implements the endpoint natively, so nothing is translated and nothing can diverge.

`★ Insight ─────────────────────────────────────`
The chat handler cannot be reused: it decodes into the chat `Request` shape and applies the generation context machinery (`contextWindow`, `num_ctx`, `resolveMaxTokens`, `kv_cache_type`). An embedding model has a fixed small context and no output to predict, so those options are at best ignored and at worst an error.

The alternative to forwarding — translating to the native `/api/embed` and rebuilding the OpenAI response — is what the researched third-party proxies do. It is strictly more code, and every line of it is a place the response can drift from Ollama's.
`─────────────────────────────────────────────────`

**Files:**
- Modify: `cmd/lattice-gateway/main.go` (add `handleEmbeddings`, register it)
- Test: `cmd/lattice-gateway/embeddings_test.go` (create)

**Interfaces:**
- Consumes: `X-Request-Id` from Task 5's frontend; `locality` stamping from Task 4; `ollamaURL`, `budgeter`, `inferenceSlots`, `acquireSlot`, `ollamaTimeout()`, `logTelemetry`, all existing.
- Produces: the gateway serves `POST /v1/embeddings`.

- [ ] **Step 1: Write the failing forwarding test**

Create `cmd/lattice-gateway/embeddings_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The request is forwarded, not translated: Ollama implements /v1/embeddings
// natively, so the body goes through untouched and the response comes back as
// Ollama wrote it. The line is keyed from the header, because an embeddings body
// carries no routing envelope to read the id from.
func TestHandleEmbeddingsForwardsToOllamaAndStampsTelemetry(t *testing.T) {
	var sawPath string
	var sawBody map[string]interface{}

	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&sawBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],"model":"embeddinggemma:latest"}`))
	}))
	defer ollama.Close()

	path := t.TempDir() + "/telemetry-gateway.jsonl"
	t.Setenv("LATTICE_GATEWAY_TELEMETRY", path)

	oldURL, oldBudgeter := ollamaURL, budgeter
	ollamaURL = ollama.URL
	// A margin nothing can cross, so CanAccommodate is deterministic rather than
	// a measurement of whatever machine runs the test.
	budgeter = &MemoryBudgeter{safeMargin: 0, pageSize: 4096}
	defer func() { ollamaURL, budgeter = oldURL, oldBudgeter }()

	req := httptest.NewRequest("POST", "/v1/embeddings",
		strings.NewReader(`{"model":"embeddinggemma:latest","input":"hello"}`))
	req.Header.Set("X-Request-Id", "req-embed")
	rec := httptest.NewRecorder()
	handleEmbeddings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if sawPath != "/v1/embeddings" {
		t.Errorf("forwarded to %q, want /v1/embeddings — the native /api/embed would need a translation", sawPath)
	}
	if sawBody["input"] != "hello" {
		t.Errorf("the client's body did not arrive intact: %v", sawBody)
	}
	if !strings.Contains(rec.Body.String(), `"embedding"`) {
		t.Errorf("the response body was not relayed: %s", rec.Body.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no telemetry line: %v", err)
	}
	var got Telemetry
	if err := json.Unmarshal(bytes.TrimSpace(raw), &got); err != nil {
		t.Fatalf("telemetry is not one JSON line: %v (%s)", err, raw)
	}
	if got.RequestID != "req-embed" {
		t.Errorf("request_id = %q, want the header's value", got.RequestID)
	}
	if got.Locality != "local" {
		t.Errorf("locality = %q, want local", got.Locality)
	}
	if got.Error != "" {
		t.Errorf("error = %q, want none", got.Error)
	}
}

// Ollama's rejections are the client's to see: a token-array input, or an empty
// encoding_format, is refused upstream and the status must be relayed rather
// than masked as a gateway fault.
func TestHandleEmbeddingsRelaysAnUpstreamErrorStatus(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid input", http.StatusBadRequest)
	}))
	defer ollama.Close()

	path := t.TempDir() + "/telemetry-gateway.jsonl"
	t.Setenv("LATTICE_GATEWAY_TELEMETRY", path)

	oldURL, oldBudgeter := ollamaURL, budgeter
	ollamaURL = ollama.URL
	budgeter = &MemoryBudgeter{safeMargin: 0, pageSize: 4096}
	defer func() { ollamaURL, budgeter = oldURL, oldBudgeter }()

	rec := httptest.NewRecorder()
	handleEmbeddings(rec, httptest.NewRequest("POST", "/v1/embeddings",
		strings.NewReader(`{"model":"embeddinggemma:latest","input":[1,2,3]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("a rejected embedding wrote no telemetry: %v", err)
	}
	var got Telemetry
	if err := json.Unmarshal(bytes.TrimSpace(raw), &got); err != nil {
		t.Fatalf("telemetry is not one JSON line: %v (%s)", err, raw)
	}
	if got.Error == "" {
		t.Error("the rejection recorded no error — the display cannot tell it from a success")
	}
}

// The check is kept despite the slot already serialising inference: with the slot
// held it is no longer a concurrency guard, but a floor on the machine being
// usable at all. A 429 naming memory pressure is a visible, correctable failure.
func TestHandleEmbeddingsRefusesUnderMemoryPressure(t *testing.T) {
	path := t.TempDir() + "/telemetry-gateway.jsonl"
	t.Setenv("LATTICE_GATEWAY_TELEMETRY", path)

	old := budgeter
	// A margin no machine can satisfy, so the refusal is deterministic.
	budgeter = &MemoryBudgeter{safeMargin: ^uint64(0), pageSize: 4096}
	defer func() { budgeter = old }()

	rec := httptest.NewRecorder()
	handleEmbeddings(rec, httptest.NewRequest("POST", "/v1/embeddings",
		strings.NewReader(`{"model":"embeddinggemma:latest","input":"hello"}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("a refused embedding wrote no telemetry: %v", err)
	}
	var got Telemetry
	if err := json.Unmarshal(bytes.TrimSpace(raw), &got); err != nil {
		t.Fatalf("telemetry is not one JSON line: %v (%s)", err, raw)
	}
	if got.Error != "memory_pressure" {
		t.Errorf("error = %q, want memory_pressure", got.Error)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/lattice-gateway/ -run TestHandleEmbeddings -v`
Expected: FAIL to compile — `undefined: handleEmbeddings`.

- [ ] **Step 3: Write the handler**

Add to `cmd/lattice-gateway/main.go`, above `handleHealth`:

```go
// handleEmbeddings forwards an embedding request to Ollama's own OpenAI-compatible
// /v1/embeddings and writes one telemetry line.
//
// It does not reuse handleInference. That handler decodes the chat Request shape
// and applies the generation context machinery — contextWindow, num_ctx,
// resolveMaxTokens, kv_cache_type — none of which an embedding model has: it has a
// fixed small context and no output to predict. Ollama implements the endpoint
// natively, so nothing is translated and the response's shape, its base64
// encoding and its precision are Ollama's by construction.
//
// The request carries no routing envelope, so its id arrives as a header. A
// caller that reaches this port directly leaves the id empty, exactly as a direct
// caller to /v1/chat/completions leaves it empty today.
func handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	requestID := r.Header.Get("X-Request-Id")

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()})
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !budgeter.CanAccommodate() {
		fmt.Println("  Memory pressure: available RAM below safety margin. Rejecting embedding to avoid swap.")
		logTelemetry(Telemetry{
			RequestID: requestID,
			Elapsed:   time.Since(t0).Seconds(),
			Error:     "memory_pressure",
		})
		http.Error(w, "Local memory pressure: available RAM below safety margin", http.StatusTooManyRequests)
		return
	}

	// The same single slot chat takes, for the same reason: with one model
	// resident, a concurrent embed evicts the resident chat model — which is the
	// swap pressure this project has already paid for once.
	if !acquireSlot(r.Context()) {
		logTelemetry(Telemetry{
			RequestID: requestID,
			Elapsed:   time.Since(t0).Seconds(),
			Error:     "client_cancelled_while_queued",
		})
		return
	}
	defer func() { <-inferenceSlots }()

	upstream, err := http.NewRequestWithContext(r.Context(), "POST",
		ollamaURL+"/v1/embeddings", bytes.NewReader(bodyBytes))
	if err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()})
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	upstream.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: ollamaTimeout()}
	resp, err := client.Do(upstream)
	if err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()})
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	te := Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds()}
	// The upstream status is the client's to interpret: an empty encoding_format
	// or a token-array input is Ollama's rejection, and masking it as a gateway
	// fault would send the caller looking in the wrong place.
	if resp.StatusCode != http.StatusOK {
		te.Error = fmt.Sprintf("upstream status %d", resp.StatusCode)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	if _, copyErr := io.Copy(w, resp.Body); copyErr != nil && te.Error == "" {
		te.Error = copyErr.Error()
	}
	logTelemetry(te)
}
```

- [ ] **Step 4: Register the route**

Replace `main`'s registration lines:

```go
func main() {
	budgeter = NewMemoryBudgeter()
	ollamaURL = latticeconfig.Env("LATTICE_OLLAMA_URL", "http://localhost:11434")

	providerMutex.Lock()
	providers["ollama"] = &OllamaProvider{Endpoint: ollamaURL}
	providerMutex.Unlock()

	addr := latticeconfig.Env("LATTICE_GATEWAY_ADDR", ":8081")
	fmt.Printf("Lattice Gateway listening on %s (Dynamic Memory Budgeting active)\n", addr)
	log.Fatal(http.ListenAndServe(addr, newRouter()))
}
```

and add, above `main`:

```go
// newRouter holds the route table so it can be asserted in a test: a route that
// exists in the source but is never registered is, from the control plane's
// side, indistinguishable from one that does not exist.
func newRouter() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleInference)
	mux.HandleFunc("/v1/embeddings", handleEmbeddings)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/telemetry", handleTelemetry)
	return mux
}
```

- [ ] **Step 5: Add the route-registration test**

Append to `cmd/lattice-gateway/embeddings_test.go`:

```go
func TestRouterServesEveryRoute(t *testing.T) {
	mux := newRouter()
	for _, path := range []string{"/v1/chat/completions", "/v1/embeddings", "/health", "/telemetry"} {
		if _, pattern := mux.Handler(httptest.NewRequest("POST", path, nil)); pattern != path {
			t.Errorf("%s is served as %q — an unregistered route is a 404 to the caller", path, pattern)
		}
	}
}
```

- [ ] **Step 6: Run the whole package**

Run: `go test ./cmd/lattice-gateway/ -v`
Expected: PASS, including the three new embeddings tests and the route test.

- [ ] **Step 7: Commit**

```bash
git add cmd/lattice-gateway/main.go cmd/lattice-gateway/embeddings_test.go
git commit -m "Serve /v1/embeddings from the gateway by forwarding to Ollama"
```

---

### Task 7: Amend the component specs

Spec §8 names every document this obliges a change in. This task does the `docs/specs/` half. Each edit is a claim that must match the code as it now stands — read the code, not this plan, if the two disagree.

**Files:**
- Modify: `docs/specs/lattice-agent-surface.md`
- Modify: `docs/specs/lattice-build-orchestration.md`
- Modify: `docs/specs/lattice-observability.md`
- Modify: `docs/specs/lattice-control.md`
- Modify: `docs/specs/lattice-frontend.md`
- Modify: `docs/specs/lattice-gateway.md`

**Interfaces:**
- Consumes: the code from Tasks 2–6.
- Produces: nothing executable.

- [ ] **Step 1: Retract the agent-surface non-goal**

In `docs/specs/lattice-agent-surface.md` §4, replace the `/v1/embeddings` entry:

```markdown
- **`/v1/embeddings`** — **retracted as a non-goal 2026-09-26.** It was deferred on
  the grounds that "the consumer that prompted this work already has a working
  embedding path". That path *was* the bypass — pointing directly at the Mac's
  Ollama — and the bypass was deliberately closed when both agent-runtime
  providers were repointed at the frontend. The premise is gone, so the non-goal
  goes with it. The route is now served by the frontend and the gateway; it is
  local-only by platform, because Ollama refuses embeddings on its cloud
  passthrough. See
  [lattice-locality-and-embeddings.md](lattice-locality-and-embeddings.md) §4.
```

- [ ] **Step 2: Correct the token claim and close the two open items**

In `docs/specs/lattice-build-orchestration.md`:

§5.1 — replace the prerequisite sentence about `/v1/embeddings`:

```markdown
- `/v1/embeddings` returns `404` through the frontend, which breaks an agent
  runtime's `auxiliary.embedding` and therefore the orchestrator's memory.
  **Resolved 2026-09-26** — the route is served. See
  [lattice-locality-and-embeddings.md](lattice-locality-and-embeddings.md) §4.
```

§5.2 — replace the claim that token accounting is unmeasurable:

```markdown
**Token accounting is partly measurable, and the earlier claim that it is
"unmeasurable" was overstated.** The gateway emits `prompt_tokens` and
`completion_tokens`, and control relays them onto the Pi verbatim, so **local**
spend is measurable today. What is genuinely absent is **cloud** spend: the
frontend reverse-proxies the response untouched and never decodes its `usage`.
Corrected 2026-09-26; see
[lattice-locality-and-embeddings.md](lattice-locality-and-embeddings.md) §5.1.
```

§9.1 and §9.3 — mark both closed, naming what closed them:

```markdown
1. ~~`/v1/embeddings` through Lattice~~ — **closed 2026-09-26**, the route is
   served (lattice-locality-and-embeddings.md §4).
```

```markdown
3. ~~Whether a target's locality should be a reported field~~ — **closed
   2026-09-26.** It should, and it is: every routing decision and every telemetry
   line now carries `locality` (lattice-locality-and-embeddings.md §3).
```

Leave the numbering intact; do not renumber the remaining open questions.

- [ ] **Step 3: Point observability §2 at the field**

In `docs/specs/lattice-observability.md` §2, replace the two requirements that locality and the token gap bear on:

```markdown
- **Routing distribution (% cloud vs % local)** — read from the `locality` field
  each stream now carries. Do not derive it from `target`: parsing a name for
  semantics is what the field exists to retire
  (lattice-locality-and-embeddings.md §3).
- **Total token spend** — **local** spend is available: the gateway emits
  `prompt_tokens` and `completion_tokens` and control relays them onto the Pi
  verbatim. **Cloud** spend is not: the frontend never decodes the response's
  `usage`. This is a named, open gap, not a general absence
  (lattice-locality-and-embeddings.md §5.1).
- **Average latency per target** — unchanged, and `target` remains the key. Note
  that control's `decision_time_s` is routing time (~0.002 s) and the frontend's
  `total_time_s` is end-to-end (~2 s); pooling them into one figure produces an
  average true of neither layer.
```

- [ ] **Step 4: Add the field to the control spec**

In `docs/specs/lattice-control.md`, in the section describing the decision payload and the telemetry schema, add:

```markdown
`locality` — `local` | `cloud` | `unknown` — is derived from the registry the
target was chosen from (`gateways` → local, `providers` → cloud) and reported on
both the decision and the telemetry line. It sits beside `target`, which stays the
source of truth. Refusals and fail-closed decisions name no target and therefore
report `unknown`. The values are the same strings the registry entries already use
in `Capabilities`, so there is no translation table between them. See
[lattice-locality-and-embeddings.md](lattice-locality-and-embeddings.md) §3.
```

- [ ] **Step 5: Document the frontend route and the header**

In `docs/specs/lattice-frontend.md`, in the route table, add:

```markdown
| `POST /v1/embeddings` | shared parse → control → proxy flow; forwards to the target's `/v1/embeddings` |
```

and, in the proxy-behaviour section:

```markdown
**The upstream path is route-dependent.** A chat request is forced to
`/v1/chat/completions`; an embedding request to `/v1/embeddings`. **The routing
envelope is injected only where a routing contract exists** — that is, the chat
path, and only when the resolved target's `locality` is `local`. An embeddings
body is forwarded without one, so its correlation id travels in an
`X-Request-Id` header instead, which the frontend sets on both routes.
```

Also replace the sentence describing the envelope's condition, which currently names the literal `mac-gateway`, so it reads `decision.Locality == "local"`.

- [ ] **Step 6: Document the gateway route and its schema**

In `docs/specs/lattice-gateway.md`, in the route table, add:

```markdown
| `POST /v1/embeddings` | forwards to Ollama's own `/v1/embeddings`; takes the single inference slot |
```

and, in the telemetry-schema section:

```markdown
Every line carries `locality: "local"`, stamped inside `logTelemetry` rather than
by callers, so no call site can omit it. The gateway has no `target` field and
does not gain one: its provenance is this process, and locality is the one
dimension the three streams share.
```

Add the embeddings behaviour:

```markdown
The embeddings route is a **forwarding** route, not a translation. Ollama
implements `/v1/embeddings` natively, so the body is passed through untouched and
the response relayed with its status and content type, which makes the response
shape, its base64 encoding and its precision Ollama's by construction. The chat
path's context machinery (`num_ctx`, `resolveMaxTokens`, `kv_cache_type`) is
deliberately not applied: an embedding model has a fixed small context and no
output to predict. The request takes the same one inference slot as a chat turn,
and the memory-budgeter check is kept, so a refusal is a visible `429` rather than
an out-of-memory event.
```

- [ ] **Step 7: Check the claim against the code, not against this plan**

Run: `grep -rn 'mac-gateway' docs/specs/ | grep -v 'lattice-locality-and-embeddings'`

Expected: any remaining hit describes `target` as **data** (a value a reader may see) or is a historical note. A hit asserting that some layer's *behaviour* depends on that name is now wrong — the only such dependency was `buildProxyBody`, and Task 3 removed it.

- [ ] **Step 8: Commit**

```bash
git add docs/specs/
git commit -m "Bring the component specs in line with reported locality and embeddings"
```

---

### Task 8: Amend the guide and the handbook

The client-facing and contributor-facing halves of spec §8. The handbook carries the telemetry-schema section and the request lifecycle; the guide carries what a client may send.

**Files:**
- Modify: `docs/guide/user-guide.md`
- Modify: `docs/handbook/architecture-handbook.md`

**Interfaces:**
- Consumes: the code from Tasks 2–6.
- Produces: nothing executable.

- [ ] **Step 1: Document the embeddings endpoint in the guide**

Add a section to `docs/guide/user-guide.md`, after the chat examples:

```markdown
### Embeddings

`POST /v1/embeddings` is supported. The body is forwarded as you send it — `model`
is rewritten to the resolved name, and nothing else is touched — which is why
`input`, `encoding_format`, and any field Lattice does not know about survive.

```bash
curl -s http://127.0.0.1:8080/v1/embeddings \
  -H 'Content-Type: application/json' \
  -d '{"model":"embeddinggemma:latest","input":"the routing rule"}'
```

Four limits are worth knowing, because they are Ollama's and Lattice relays them
rather than smoothing them over:

- **Embeddings are local-only.** Ollama refuses them on its cloud passthrough, and
  no `-cloud` embedding tag exists. The embedding model runs on the Mac.
- **`input` as a token array returns `400`.** Send text; Ollama diverges from
  OpenAI here.
- **An empty `encoding_format` (`""`) is rejected.** Omit the key rather than
  sending an empty one — the body is forwarded, not sanitised.
- **base64 embeddings are raw float32, little-endian**, standard encoding.

An embedding shares the gateway's single inference slot with chat, so an embed
issued during a long turn waits for it. That is deliberate: with one model
resident, a concurrent embed would evict the chat model.
```

- [ ] **Step 2: Document the attribute in the handbook**

In `docs/handbook/architecture-handbook.md`, in the telemetry-pipeline section, add:

```markdown
**`locality` is reported, never derived.** Every routing decision and every
telemetry line carries `locality` — `local`, `cloud`, or `unknown` — taken from
the registry the target was chosen from, not from the target's name. The values
are the same strings the registry entries use in `Capabilities`, so there is one
vocabulary and no translation table to drift. Consumers must treat the set as
open: funnel an unrecognised value into `unknown` rather than dropping the event,
and always retain `target`, which is never dropped.

This exists because the alternative — each tool matching the two literal target
names — puts the registry's knowledge in every consumer that needs it. Adding a
value to the enum is non-breaking; changing or removing one is not.
```

and, in the request-lifecycle section:

```markdown
An embeddings request takes the same path as a chat request — frontend parses,
control decides, the target executes — and differs in two ways: the upstream path
is `/v1/embeddings` rather than `/v1/chat/completions`, and no routing envelope is
attached, because that object is the chat translation layer's contract. Its
correlation id travels in an `X-Request-Id` header instead, so the request is
still keyed across all three telemetry streams.
```

- [ ] **Step 3: Check the invariant list**

Read the handbook's "invariants that must not drift" section. If it states that the frontend decides the routing envelope by target name, correct it to locality. If it states that only chat is routable, add embeddings.

- [ ] **Step 4: Commit**

```bash
git add docs/guide/user-guide.md docs/handbook/architecture-handbook.md
git commit -m "Document the embeddings route and the reported locality attribute"
```

---

### Task 9: Build, deploy, and verify end to end

The order is control → frontend → gateway. A frontend running ahead of control reads an empty `locality` and withholds the routing envelope from a local target: it fails safe, but the gateway's line loses its key until control catches up.

**Files:** none in the repository. This task produces a verified deployment and a recorded result.

**Interfaces:**
- Consumes: Tasks 2–6.
- Produces: the acceptance evidence for spec §8.

- [ ] **Step 1: Build and test everything, from a clean tree**

```bash
gofmt -l cmd/ pkg/
go build ./...
go vet ./...
go test ./...
```

Expected: `gofmt -l` prints nothing, build and vet are silent, all tests pass.

- [ ] **Step 2: Cross-compile for both targets**

```bash
GOOS=darwin GOARCH=arm64 go build -o bin/lattice-gateway  ./cmd/lattice-gateway
GOOS=linux  GOARCH=arm64 go build -o bin/lattice-control  ./cmd/lattice-control
GOOS=linux  GOARCH=arm64 go build -o bin/lattice-frontend ./cmd/lattice-frontend
```

- [ ] **Step 3: Deploy control, then frontend, to the Pi**

Overwriting a running binary fails with `Text file busy`, so stop, copy, start — one component at a time, control first.

```bash
systemctl --user stop lattice-control
scp bin/lattice-control <rpi4>:~/bin/lattice-control.new
ssh <rpi4> 'mv ~/bin/lattice-control.new ~/bin/lattice-control'
systemctl --user start lattice-control
```

Then the same four lines for `lattice-frontend`. If the units belong to another account, `systemctl --user --machine=<lattice-account>@.host` is required — a bare `--user` from a different login answers about the wrong manager and reports `inactive` for a service that is running.

- [ ] **Step 4: Deploy the gateway to the Mac**

```bash
launchctl bootout gui/$UID/com.lattice.gateway
cp bin/lattice-gateway <gateway-binary-path>
launchctl bootstrap gui/$UID <plist-path>
```

Or re-run `./deploy/install-macos.sh`, which boots out before re-bootstrapping. Allow up to ~10 s before local requests route again: control re-probes the gateway on that interval.

- [ ] **Step 5: Verify an embedding end to end**

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/v1/embeddings \
  -H 'Content-Type: application/json' \
  -d '{"model":"embeddinggemma:latest","input":"the routing rule"}'
```

Expected: `200`. This is the check that closes the `404` — if it returns `404`, the route is not registered in the binary that is running; re-check Step 3.

Then confirm the payload is real:

```bash
curl -s http://127.0.0.1:8080/v1/embeddings \
  -H 'Content-Type: application/json' \
  -d '{"model":"embeddinggemma:latest","input":"the routing rule"}' \
  | head -c 200
```

Expected: an OpenAI-shaped list whose `data[0].embedding` is a numeric array.

- [ ] **Step 6: Verify all three streams carry the locality**

Run an embedding and a chat request, then read each stream:

```bash
ssh <rpi4> 'tail -n 3 /var/log/lattice/telemetry-control.jsonl'
ssh <rpi4> 'tail -n 3 /var/log/lattice/telemetry-frontend.jsonl'
ssh <rpi4> 'tail -n 3 /var/log/lattice/telemetry-gateway.jsonl'
```

Expected: every line of all three carries `"locality":"local"` for these requests, and the lines share one `request_id`. The gateway's line is the one relayed from the Mac, so this also proves the relay carries the new field without a change of its own.

Then verify the other two values:

```bash
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"minimax-m3:cloud","messages":[{"role":"user","content":"hi"}]}' \
  -o /dev/null
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"minimax-m3:cloud","messages":[{"role":"user","content":"hi"}],"routing":{"privacy":"LOCAL_ONLY"}}' \
  -o /dev/null
```

Expected: the first is `200` and its control line reads `"locality":"cloud"`; the second is `409` and its control **and** frontend lines read `"locality":"unknown"`. A refusal is precisely where the field must still be countable.

- [ ] **Step 7: Verify nothing regressed**

```bash
curl -s http://127.0.0.1:8080/v1/models | head -c 200
curl -s http://127.0.0.1:8080/health
curl -s http://127.0.0.1:8082/status
curl -s http://<mac-tailnet-address>:8081/health
```

Expected: the capability list, `OK`, per-gateway health JSON, and `{"status":"ok","max_context":...}`. Then run one real chat completion and one streaming completion through the frontend and confirm both still answer.

- [ ] **Step 8: Commit nothing, and record the result**

This task changes no tracked file. Record the observed status codes and telemetry lines in the plan-execution ledger, not in the repository.

---

### Task 10: Measure the embedding reload cost

Spec §4.7 accepts that interleaving an embedding with a chat turn evicts the resident chat model, and §9.2 requires the cost to be **measured**, not assumed. This is that measurement.

**Files:**
- Modify: `docs/specs/lattice-locality-and-embeddings.md` (§4.7 and §9.2)

**Interfaces:**
- Consumes: the deployed route from Task 9.
- Produces: a recorded figure, and either "accepted as measured" or a named follow-up.

- [ ] **Step 1: Warm the chat model, then measure a turn**

```bash
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"granite4:3b","messages":[{"role":"user","content":"Say OK"}],"stream":false}' \
  -o /dev/null
```

Then read the gateway's last line and record `elapsed_s`, `prompt_tokens`, and `completion_tokens`. This is the **warm** figure: no reload.

- [ ] **Step 2: Evict, then measure the same turn again**

```bash
curl -s http://127.0.0.1:8080/v1/embeddings \
  -H 'Content-Type: application/json' \
  -d '{"model":"embeddinggemma:latest","input":"the routing rule"}' -o /dev/null
```

then repeat Step 1's chat request and read the gateway's line again. This is the **cold** figure.

- [ ] **Step 3: Compute the reload cost**

The difference between the two `elapsed_s` values is the reload penalty an interleave costs. Record both figures, the embedding's own `elapsed_s`, and the model names — a figure without the model it was measured on is not a measurement.

- [ ] **Step 4: Record it in the spec**

Replace §4.7's last paragraph:

```markdown
Measured on 2026-09-26 against `<chat model>` with `<embedding model>` on the Mac:
a warm chat turn took `<N>` s, the embedding `<N>` s, and the same chat turn
immediately after the embedding `<N>` s — a reload penalty of **`<N>` s**, against
a turn whose prefill already dominates. [Then state which of the two conclusions
the numbers support: the cost is small enough that §4.7's acceptance stands
unchanged, or it is large enough that the caller-side mitigations named in §9.2
become the recommendation.]

Do not restate an estimate here. If the figure is not measured, this section
says so.
```

and close §9.2:

```markdown
2. ~~The embedding reload cost.~~ **Measured 2026-09-26 — see §4.7.** The
   mitigation, if one is wanted, is on the caller's side (batching, or fewer
   recall calls); it is not an unmeasured policy change here.
```

- [ ] **Step 5: Commit**

```bash
git add docs/specs/lattice-locality-and-embeddings.md
git commit -m "Record the measured cost of an embedding interleave"
```

---

## Self-review

**Spec coverage.**

| spec section | task |
|---|---|
| §2 the verified-facts table | Task 1 corrects the rows it is wrong about; Tasks 2–6 make the rest true |
| §3.1 the attribute, its values, its rule, its default | Task 2 (`localityFor`), Task 2 steps 5–7 |
| §3.2 where it is emitted (four producers) | Task 2 (control ×2), Task 3 (frontend), Task 4 (gateway) |
| §3.3 a field beside the name, never instead of it | Task 2 step 5 keeps `target`; nothing removes it |
| §3.4 consumers must not derive it | Task 3 (the frontend's literal goes), Task 1 step 4 (stats is a future reader) |
| §3.5 `unknown` and forward compatibility | Task 2 (`localityFor` defaults to `unknown`), Task 3 step 8 (the frontend defaults it too) |
| §3.6 what it retires | Task 3 |
| §4.1 why embeddings are local-only | No routing change is needed; Task 5 step 5 records why |
| §4.2 frontend: shared flow, route-dependent path, no envelope | Task 5 |
| §4.3 gateway: forward to Ollama, do not reuse the chat handler | Task 6 |
| §4.4 the single slot, and the budgeter check kept | Task 6 step 3, and its two guard tests |
| §4.5 what is not reused | Task 6 step 3 — the handler applies no context machinery |
| §4.6 wire notes | Task 8 step 1 (the guide carries all four), Task 6's error-relay test |
| §4.7 the tradeoff, stated honestly | Task 10 measures it |
| §5.1–§5.3 corrections to the record | Task 1, Task 7 step 2 |
| §6 non-goals | Global Constraints (no cache, no queue, no store, no cloud route) |
| §7 invariant check | Global Constraints (one slot, stdlib only, no new infrastructure) |
| §8 amendments required elsewhere | Task 7 (specs), Task 8 (guide, handbook); the README row landed with the spec |
| §9 open questions | §9.1 `locality` to the client — deliberately untouched, no consumer wants it; §9.2 measured in Task 10; §9.3–§9.5 out of scope and named |
| §8's acceptance checks | Task 9 |

**Placeholder scan.** Every code step carries the code. The only deferred values are the four measured numbers in Task 10 and the addresses in Task 9 — all four are measurements or host-specific paths that cannot exist in a tracked file, and Task 10 step 4 states plainly that an unmeasured figure must be recorded as unmeasured rather than estimated.

**Type consistency.** `localityFor(targetID string) string` is defined once, in Task 2, and named identically in Tasks 2 and the self-review. `buildProxyBody` has 4 parameters in Tasks 3 and 5 parameters in Task 5 onward, and Task 5 step 8 updates every call site in the same task. `proxyInference`, `handleEmbeddings`, and `newRouter` are spelled identically in Tasks 5 and 6 (different packages, deliberately the same names) and in their tests. The `X-Request-Id` header is set in Task 5 step 4 and read in Task 6 step 3 under the same spelling. `locality` is the field name in all three structs (control `Decision` and `Telemetry`, frontend `Decision` and `Telemetry`, gateway `Telemetry`) and the same string in JSON.

**Gaps.** Two, both deliberate. `lattice-stats` is not touched — its rewrite is Task 5 of the build-orchestration plan, and this plan's job is only to make the field available to it. And cloud token spend (§5.1, §9.4) is named in the observability spec, not delivered here, because which layer should decode `usage` is still an open decision.

**Cross-plan conflict.** The build-orchestration plan's Task 5 rewrites `cmd/lattice-stats/main.go` against observability §2, and its Task 5 step 4 asserts that `grep -rn 'prompt_tokens\|completion_tokens' cmd/lattice-stats/` returns nothing. Neither is touched here. Its prerequisite section, however, states that `/v1/embeddings` returns `404` and that neither telemetry layer emits token fields — Task 7 step 2 corrects both claims, which is why that step is in this plan rather than left to a later reconciliation.

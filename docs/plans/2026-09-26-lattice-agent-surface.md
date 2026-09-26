# Lattice Agent Surface Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an OpenAI-shaped agent runtime (tool calling, literal model names, discovery probes) be pointed at the Lattice frontend and work, without adding a second public contract.

**Architecture:** Extend the three existing binaries in place. The frontend stops rebuilding the proxy body and forwards the client's own body with only `model` and `routing` rewritten; the control plane resolves a model name to an alias *or* passes it through verbatim instead of blanking it; the gateway forwards `tools` to Ollama and translates `tool_calls` back to OpenAI shape. Two read-only discovery endpoints are added. No new process, no new infrastructure.

**Tech Stack:** Go (stdlib only — `net/http`, `encoding/json`, `httptest` for tests), Ollama native API (`/api/chat`), systemd user units (Pi) and a launchd LaunchAgent (Mac).

**Spec:** [`docs/specs/lattice-agent-surface.md`](../specs/lattice-agent-surface.md)

## Global Constraints

- **No new process and no new infrastructure.** No message bus, no database, no model inventory, no orchestrator. Everything extends the three existing binaries.
- **One public entry point.** Clients talk to the frontend only. Do not add a shim process in front of it.
- **Go stdlib only.** No new dependencies.
- **No usernames, hostnames, or addresses in tracked files.** Use environment variables and `<placeholders>`.
- **`LOCAL_ONLY` never falls back to cloud.** Do not touch the routing policy table.
- **The Mac is the only host permitted to run local models.** The Pi is cloud-only.
- **The frontend may not enumerate which body fields a client may send.** That is the bug being fixed.
- **`model_name` is never empty.** If the client named a model, that name reaches the target.
- Commit message style follows the repo: imperative mood, sentence case, no `feat:`/`fix:` prefix (see `git log`).

---

## File Structure

| file | responsibility | change |
|---|---|---|
| `cmd/lattice-frontend/main.go` | public contract: body transform, discovery, correlation id | modify |
| `cmd/lattice-frontend/handler_test.go` | tests for the above | **create** |
| `cmd/lattice-control/main.go` | model resolution, capability exposure, gateway health parsing | modify |
| `cmd/lattice-control/route_test.go` | table tests for model resolution | **create** |
| `cmd/lattice-gateway/main.go` | Ollama translation incl. tools; health payload; context ceiling | modify |
| `cmd/lattice-gateway/tools_test.go` | tool-call translation tests | **create** |
| `docs/specs/*.md`, `docs/handbook/*.md`, `docs/guide/*.md` | spec conformance and the recorded drift | modify |

Ordering matters in one place: **Task 4 must follow Task 3**, because the gateway's health payload (part of Task 4) is what single-sources the context ceiling that `/v1/models` advertises. Everything else is independent.

---

### Task 1: Frontend body passthrough

The conformance fix. `docs/specs/lattice-frontend.md` §2 already says "update the `model` field in the JSON body"; the code rebuilds the body instead and silently drops every field it does not name.

**Files:**
- Modify: `cmd/lattice-frontend/main.go:97-119`
- Test: `cmd/lattice-frontend/handler_test.go` (create)

**Interfaces:**
- Consumes: the existing `Decision` struct (`Target`, `Endpoint`, `ModelName`) and `Routing` struct (`RequestID`, `ProviderParams`) from `cmd/lattice-frontend/main.go`.
- Produces: `func buildProxyBody(raw []byte, decision Decision, requestID string, providerParams map[string]interface{}) ([]byte, error)` — used by Task 2's integration test.

- [ ] **Step 1: Write the failing test**

Create `cmd/lattice-frontend/handler_test.go`:

```go
package main

import (
	"encoding/json"
	"testing"
)

// The frontend rebuilds nothing: it forwards the client's own body with only
// model and routing rewritten. Rebuilding dropped `tools` silently, which is
// fatal for an agent, so these cases pin the fields a client actually sends.
func TestBuildProxyBodyPreservesClientFields(t *testing.T) {
	raw := []byte(`{
		"model": "local-coder",
		"messages": [{"role": "user", "content": "hi"}],
		"stream": true,
		"temperature": 0.2,
		"max_tokens": 64,
		"stop": ["\n\n"],
		"tools": [{"type": "function", "function": {"name": "get_weather"}}],
		"tool_choice": "auto"
	}`)
	decision := Decision{Target: "mac-gateway", Endpoint: "http://example:8081", ModelName: "hermes3:8b"}

	out, err := buildProxyBody(raw, decision, "rid-1", map[string]interface{}{"reasoning_effort": "low"})
	if err != nil {
		t.Fatalf("buildProxyBody: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}

	if got["model"] != "hermes3:8b" {
		t.Errorf("model = %v, want the resolved name", got["model"])
	}
	for _, field := range []string{"messages", "stream", "temperature", "max_tokens", "stop", "tools", "tool_choice"} {
		if _, ok := got[field]; !ok {
			t.Errorf("%s was dropped — the frontend must not enumerate fields", field)
		}
	}
	if got["temperature"] != 0.2 {
		t.Errorf("temperature = %v, want 0.2", got["temperature"])
	}

	routing, ok := got["routing"].(map[string]interface{})
	if !ok {
		t.Fatalf("routing missing on the local path: %v", got["routing"])
	}
	if routing["request_id"] != "rid-1" {
		t.Errorf("routing.request_id = %v, want rid-1", routing["request_id"])
	}
	if params, ok := routing["provider_params"].(map[string]interface{}); !ok || params["reasoning_effort"] != "low" {
		t.Errorf("routing.provider_params = %v, want reasoning_effort low", routing["provider_params"])
	}
}

// A client-supplied routing envelope must never reach the cloud: cloud endpoints
// speak plain OpenAI and reject the extension object.
func TestBuildProxyBodyStripsRoutingOnCloudPath(t *testing.T) {
	raw := []byte(`{
		"model": "local-brain",
		"messages": [{"role": "user", "content": "hi"}],
		"routing": {"privacy": "CLOUD_ALLOWED", "request_id": "client-chose-this"}
	}`)
	decision := Decision{Target: "ollama-cloud-primary", Endpoint: "http://example:11434", ModelName: "gemma4:31b-cloud"}

	out, err := buildProxyBody(raw, decision, "rid-2", nil)
	if err != nil {
		t.Fatalf("buildProxyBody: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if _, ok := got["routing"]; ok {
		t.Errorf("routing leaked to the cloud path: %v", got["routing"])
	}
	if got["model"] != "gemma4:31b-cloud" {
		t.Errorf("model = %v, want the resolved name", got["model"])
	}
}

// A body that is not a JSON object cannot be rewritten; the caller must be told
// rather than sending the target something malformed.
func TestBuildProxyBodyRejectsNonObject(t *testing.T) {
	if _, err := buildProxyBody([]byte(`"just a string"`), Decision{ModelName: "x"}, "rid", nil); err == nil {
		t.Error("expected an error for a non-object body")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/lattice-frontend/ -run TestBuildProxyBody -v`
Expected: FAIL — `undefined: buildProxyBody`.

- [ ] **Step 3: Write minimal implementation**

In `cmd/lattice-frontend/main.go`, add below the `Decision` type:

```go
// buildProxyBody forwards the client's own body with only two rewrites: the
// resolved model name, and the routing envelope (which belongs to the local
// translation layer only — a cloud endpoint speaks plain OpenAI and rejects it).
//
// It deliberately does not enumerate the fields it forwards. Enumerating is what
// broke this: the previous version rebuilt the body from model, messages and
// stream, so `tools` was dropped and an agent lost the ability to call anything.
func buildProxyBody(raw []byte, decision Decision, requestID string, providerParams map[string]interface{}) ([]byte, error) {
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if body == nil {
		return nil, fmt.Errorf("request body is not a JSON object")
	}

	body["model"] = decision.ModelName

	// The client's own routing envelope is never forwarded: on the local path it
	// is replaced by the one the frontend issues, and on the cloud path it must
	// not appear at all.
	delete(body, "routing")
	if decision.Target == "mac-gateway" {
		routing := map[string]interface{}{"request_id": requestID}
		if providerParams != nil {
			routing["provider_params"] = providerParams
		}
		body["routing"] = routing
	}

	return json.Marshal(body)
}
```

Then replace the body-rebuild block in `handleChat` (currently `proxyBody := make(map[string]interface{})` through `finalBodyBytes, _ := json.Marshal(proxyBody)`):

```go
	// 2. Rewrite request for Target
	finalBodyBytes, err := buildProxyBody(bodyBytes, decision, req.Routing.RequestID, req.Routing.ProviderParams)
	if err != nil {
		http.Error(w, "Malformed request body", http.StatusBadRequest)
		return
	}
```

The `req` typed parse at the top of `handleChat` stays — it is how the frontend reads `routing` without enumerating the forwarded body.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/lattice-frontend/ -v`
Expected: PASS (all three new tests plus any existing).

- [ ] **Step 5: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add cmd/lattice-frontend/main.go cmd/lattice-frontend/handler_test.go
git commit -m "Frontend: forward the client body instead of rebuilding it

The proxy rebuilt the body from model, messages and stream, so every other
field was dropped silently -- including tools, which an agent needs to
function. lattice-frontend.md already specified rewriting only the model
field, so this is a return to spec rather than a new feature."
```

---

### Task 2: Correlation id the client does not have to supply

An agent runtime has never heard of Lattice and will not send `routing.request_id`, so its runs land on the Bee screen with a blank key. The frontend generates one and echoes it.

**Files:**
- Modify: `cmd/lattice-frontend/main.go` (`handleChat`, `main`)
- Test: `cmd/lattice-frontend/handler_test.go`

**Interfaces:**
- Consumes: `buildProxyBody` from Task 1.
- Produces: `func resolveRequestID(r Routing) string`; the `X-Request-Id` response header.

- [ ] **Step 1: Write the failing tests**

First widen the test file's import block to:

```go
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)
```

Then append to `cmd/lattice-frontend/handler_test.go`:

```go
func TestResolveRequestIDKeepsAClientSuppliedID(t *testing.T) {
	if got := resolveRequestID(Routing{RequestID: "client-id"}); got != "client-id" {
		t.Errorf("got %q, want the client's own id", got)
	}
}

func TestResolveRequestIDGeneratesWhenAbsent(t *testing.T) {
	got := resolveRequestID(Routing{})
	if got == "" {
		t.Fatal("generated id is empty — telemetry would be uncorrelatable")
	}
	if other := resolveRequestID(Routing{}); other == got {
		t.Errorf("two generated ids collided (%q) — they must be unique per request", got)
	}
}

// The id must be set before the control call, not merely before the proxy call:
// control is posted the typed request, so an id assigned later is one control
// never logged and the run is correlatable in two planes out of three.
func TestHandleChatCorrelatesAllThreePlanes(t *testing.T) {
	var sawControlID, sawTargetID string

	// Declared before the control handler that closes over it: the closure runs
	// at request time, but the name must already be in scope for it to compile.
	var target *httptest.Server

	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got Request
		json.NewDecoder(r.Body).Decode(&got)
		sawControlID = got.Routing.RequestID
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Decision{Target: "mac-gateway", Endpoint: target.URL, ModelName: "hermes3:8b"})
	}))
	defer control.Close()

	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]interface{}
		json.NewDecoder(r.Body).Decode(&got)
		if routing, ok := got["routing"].(map[string]interface{}); ok {
			sawTargetID, _ = routing["request_id"].(string)
		}
		if _, ok := got["tools"]; !ok {
			t.Error("tools did not survive the frontend")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-x","choices":[]}`))
	}))
	defer target.Close()

	oldControl, oldTelemetry := controlURL, telemetryPath
	controlURL = control.URL
	telemetryPath = t.TempDir() + "/telemetry-frontend.jsonl"
	defer func() { controlURL, telemetryPath = oldControl, oldTelemetry }()

	body := `{"model":"local-coder","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleChat(rec, req)

	header := rec.Header().Get("X-Request-Id")
	if header == "" {
		t.Fatal("no X-Request-Id header on the response")
	}
	if sawControlID != header {
		t.Errorf("control saw %q, response advertised %q", sawControlID, header)
	}
	if sawTargetID != header {
		t.Errorf("target saw %q, response advertised %q", sawTargetID, header)
	}

	logged, err := os.ReadFile(telemetryPath)
	if err != nil || !strings.Contains(string(logged), header) {
		t.Errorf("frontend telemetry does not carry %q: %v %s", header, err, logged)
	}
}
```

This requires the telemetry path to be a variable. In `main.go`, change `logTelemetry` to read a package var:

```go
var telemetryPath = latticeconfig.Env("LATTICE_FRONTEND_TELEMETRY", "/var/log/lattice/telemetry-frontend.jsonl")
```

and use it in place of the inline `latticeconfig.Env(...)` call.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/lattice-frontend/ -run 'TestResolveRequestID|TestHandleChat' -v`
Expected: FAIL — `undefined: resolveRequestID`.

- [ ] **Step 3: Write minimal implementation**

Add to `cmd/lattice-frontend/main.go`:

```go
// resolveRequestID uses the client's id when it supplied one, and otherwise
// mints one. An agent runtime has never heard of Lattice, so requiring the field
// meant its runs reached the Bee screen with a blank correlation key. The
// generated id is time-based on purpose: it sorts, so the display reads
// chronologically without parsing.
func resolveRequestID(r Routing) string {
	if r.RequestID != "" {
		return r.RequestID
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}
```

In `handleChat`, immediately after the `json.Unmarshal` error check:

```go
	// Assigned before the control call so control logs the same id the gateway
	// and the frontend will use.
	req.Routing.RequestID = resolveRequestID(req.Routing)
```

And immediately before `proxy.ServeHTTP(w, r)`:

```go
	w.Header().Set("X-Request-Id", req.Routing.RequestID)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/lattice-frontend/ -v`
Expected: PASS.

- [ ] **Step 5: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add cmd/lattice-frontend/main.go cmd/lattice-frontend/handler_test.go
git commit -m "Frontend: always correlate a request, even when the client has no id"
```

---

### Task 3: Model resolution — alias or literal, never blank

Today `capabilities[req.Model]` on an unrecognised name returns the zero value `""`, so a literal model name fails anonymously with no model in telemetry. This is the gap that would bite first: `hermes3:8b` is exactly `local-coder`'s local model, the name a person is most likely to type.

**Files:**
- Modify: `cmd/lattice-control/main.go:489`, `cmd/lattice-control/main.go:500`
- Test: `cmd/lattice-control/route_test.go` (create)

**Interfaces:**
- Produces: `func resolveModel(model, target string) string` where `target` is `"cloud"` or `"local"`.

- [ ] **Step 1: Write the failing test**

Create `cmd/lattice-control/route_test.go`:

```go
package main

import "testing"

// The client's model field means one of two things: a capability alias, in which
// case policy picks the model, or a literal model name, in which case the client
// has picked it and we must not quietly replace it with nothing.
func TestResolveModel(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		target string
		want   string
	}{
		{"alias resolves to the local model", "local-brain", "local", "granite4:3b"},
		{"alias resolves to the cloud model", "local-brain", "cloud", "gemma4:31b-cloud"},
		{"second alias resolves locally", "local-coder", "local", "hermes3:8b"},
		{"second alias resolves on cloud", "local-coder", "cloud", "deepseek-v4-pro:cloud"},
		// The regression this exists for: a literal name must pass through,
		// because returning "" makes the failure anonymous.
		{"literal local model passes through", "hermes3:8b", "local", "hermes3:8b"},
		{"literal cloud model passes through", "deepseek-v4-flash:cloud", "cloud", "deepseek-v4-flash:cloud"},
		{"unknown name is not invented", "some-model:latest", "local", "some-model:latest"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveModel(tc.model, tc.target); got != tc.want {
				t.Errorf("resolveModel(%q, %q) = %q, want %q", tc.model, tc.target, got, tc.want)
			}
		})
	}
}

// An empty model is the one case that cannot be resolved to anything: the
// frontend rejects it, and control must not paper over it with a guess.
func TestResolveModelEmptyStaysEmpty(t *testing.T) {
	if got := resolveModel("", "local"); got != "" {
		t.Errorf("resolveModel(\"\", local) = %q, want \"\"", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/lattice-control/ -run TestResolveModel -v`
Expected: FAIL — `undefined: resolveModel`.

- [ ] **Step 3: Write minimal implementation**

Add to `cmd/lattice-control/main.go`, above `handleRoute`:

```go
// resolveModel maps the client's model field to the name actually sent to the
// target.
//
// A capability alias is a request for policy to choose the model, so it is
// resolved per target. Anything else is a literal model name the client chose,
// and it is passed through verbatim — the previous code looked the name up in
// the capability map and used the zero value on a miss, so a literal model
// arrived at the target as an empty string and failed with nothing in telemetry
// naming what had been asked for. The rule is that model_name is never blank
// when the client named something.
func resolveModel(model, target string) string {
	c, ok := capabilities[model]
	if !ok {
		return model
	}
	if target == "cloud" {
		return c.cloud
	}
	return c.local
}
```

Then in `handleRoute`, replace the two assignments:

```go
			modelName = capabilities[req.Model].cloud
```
with
```go
			modelName = resolveModel(req.Model, "cloud")
```

and

```go
						modelName = capabilities[req.Model].local
```
with
```go
						modelName = resolveModel(req.Model, "local")
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/lattice-control/ -v`
Expected: PASS (new tests plus the existing `relay_test.go` cases).

- [ ] **Step 5: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add cmd/lattice-control/main.go cmd/lattice-control/route_test.go
git commit -m "Control: pass an unrecognised model through instead of blanking it

An unknown name resolved to the capability map's zero value, so a literal
model reached the target as an empty string and failed with nothing in
telemetry naming what was asked for."
```

---

### Task 4: Discovery surface

A service that implements one route and offers no discovery is indistinguishable from a service that does not exist to any client that probes first — and most do. This misread has already happened once against this system.

The context ceiling is **single-sourced**: the gateway reports its own effective ceiling in `/health`, control stores it alongside health, and `/v1/models` advertises it. Two environment variables that must agree is exactly the drift this project's docs keep recording.

**Files:**
- Modify: `cmd/lattice-gateway/main.go` (`handleHealth`)
- Modify: `cmd/lattice-control/main.go` (`monitorHealth`, `main`)
- Modify: `cmd/lattice-frontend/main.go` (`main`)
- Test: `cmd/lattice-frontend/handler_test.go`, `cmd/lattice-control/route_test.go`

**Interfaces:**
- Produces: gateway `GET /health` → `{"status":"ok","max_context":N}`; control `GET /capabilities` → `{"context_length":N,"capabilities":[{"id","local","cloud"}]}`; frontend `GET /v1/models` and `GET /health`.
- Consumes: `resolveModel` and the `capabilities` map from Task 3.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/lattice-frontend/handler_test.go`:

```go
func TestHandleModelsListsTheCapabilityNamespace(t *testing.T) {
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/capabilities" {
			t.Errorf("frontend asked control for %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"context_length":65536,"capabilities":[{"id":"local-brain","local":"granite4:3b","cloud":"gemma4:31b-cloud"}]}`))
	}))
	defer control.Close()

	old := capabilitiesURL
	capabilitiesURL = control.URL + "/capabilities"
	defer func() { capabilitiesURL = old }()

	rec := httptest.NewRecorder()
	handleModels(rec, httptest.NewRequest("GET", "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q — Go sniffs JSON as text/plain without an explicit header", ct)
	}

	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID            string `json:"id"`
			Object        string `json:"object"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not OpenAI list JSON: %v", err)
	}
	if got.Object != "list" || len(got.Data) != 1 || got.Data[0].ID != "local-brain" {
		t.Fatalf("unexpected model list: %+v", got)
	}
	if got.Data[0].ContextLength != 65536 {
		t.Errorf("context_length = %d, want the gateway's real ceiling", got.Data[0].ContextLength)
	}
}

// Discovery must fail closed rather than invent a namespace: a client that gets
// a model list it cannot use is worse served than one that is told to wait.
func TestHandleModelsFailsClosed(t *testing.T) {
	old := capabilitiesURL
	capabilitiesURL = "http://127.0.0.1:1/capabilities"
	defer func() { capabilitiesURL = old }()

	rec := httptest.NewRecorder()
	handleModels(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code == http.StatusOK {
		t.Error("expected a failure status when control is unreachable")
	}
}

func TestHandleHealth(t *testing.T) {
	rec := httptest.NewRecorder()
	handleHealth(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
```

Append to `cmd/lattice-control/route_test.go`:

```go
func TestHandleCapabilitiesIsSortedAndReportsTheCeiling(t *testing.T) {
	healthMutex.Lock()
	gatewayMaxContext = 65536
	healthMutex.Unlock()

	rec := httptest.NewRecorder()
	handleCapabilities(rec, httptest.NewRequest("GET", "/capabilities", nil))

	var got struct {
		ContextLength int `json:"context_length"`
		Capabilities  []struct {
			ID string `json:"id"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if got.ContextLength != 65536 {
		t.Errorf("context_length = %d, want 65536", got.ContextLength)
	}
	if len(got.Capabilities) != 2 {
		t.Fatalf("got %d capabilities, want 2", len(got.Capabilities))
	}
	// Sorted so the list a client sees does not change between calls.
	if got.Capabilities[0].ID != "local-brain" || got.Capabilities[1].ID != "local-coder" {
		t.Errorf("capabilities not sorted: %+v", got.Capabilities)
	}
}
```

Add these imports to `route_test.go`: `encoding/json`, `net/http/httptest`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/lattice-control/ ./cmd/lattice-frontend/ -v`
Expected: FAIL — `undefined: handleCapabilities`, `undefined: handleModels`, `undefined: gatewayMaxContext`.

- [ ] **Step 3: Implement the gateway's health payload**

In `cmd/lattice-gateway/main.go`, replace the body of `handleHealth`'s success path:

```go
func handleHealth(w http.ResponseWriter, r *http.Request) {
	// Health check: is Ollama responsive AND is memory okay?
	if !budgeter.CanAccommodate() {
		http.Error(w, "Memory pressure high", http.StatusServiceUnavailable)
		return
	}

	resp, err := http.Get(ollamaURL + "/api/tags")
	if err != nil || resp.StatusCode != http.StatusOK {
		http.Error(w, "Ollama unhealthy", http.StatusServiceUnavailable)
		return
	}

	// The ceiling is reported rather than configured twice: the gateway is the
	// only process that knows what a context window costs in KV cache here, so
	// it is the authority on the number and the control plane relays it.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "ok",
		"max_context": maxContext,
	})
}
```

- [ ] **Step 4: Implement control's health parsing and `/capabilities`**

In `cmd/lattice-control/main.go`, add to the var block holding `gatewayTelemetrySeq`:

```go
	// gatewayMaxContext is what the gateway reports it will honour, refreshed on
	// each health poll. It is advertised by /capabilities so the limit is
	// discoverable rather than invisible.
	gatewayMaxContext int
```

In `monitorHealth`, replace the health-check branch so a 200 also decodes the payload:

```go
		for id, gw := range gateways {
			resp, err := http.Get(gw.Endpoint + "/health")
			healthy := err == nil && resp != nil &&
				(resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound)
			if healthy && resp.StatusCode == http.StatusOK {
				var h struct {
					MaxContext int `json:"max_context"`
				}
				if json.NewDecoder(resp.Body).Decode(&h) == nil && h.MaxContext > 0 {
					healthMutex.Lock()
					gatewayMaxContext = h.MaxContext
					healthMutex.Unlock()
				}
			}
			if resp != nil {
				resp.Body.Close()
			}
			healthMutex.Lock()
			gatewayHealthy[id] = healthy
			healthMutex.Unlock()
			pullGatewayTelemetry(gw)
		}
```

Add the handler above `handleRoute`:

```go
// handleCapabilities exposes the client-facing namespace — the capability
// aliases — plus the ceiling the gateway will actually honour. A probing client
// that finds this stops concluding the API is absent, which is what happened
// when the only route was the chat endpoint.
func handleCapabilities(w http.ResponseWriter, r *http.Request) {
	healthMutex.RLock()
	ctxCap := gatewayMaxContext
	healthMutex.RUnlock()

	ids := make([]string, 0, len(capabilities))
	for id := range capabilities {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	list := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		list = append(list, map[string]string{
			"id":    id,
			"local": capabilities[id].local,
			"cloud": capabilities[id].cloud,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"context_length": ctxCap,
		"capabilities":   list,
	})
}
```

Add `"sort"` to control's imports, and register the route in `main`:

```go
	http.HandleFunc("/capabilities", handleCapabilities)
```

- [ ] **Step 5: Implement the frontend's discovery endpoints**

In `cmd/lattice-frontend/main.go`, add:

```go
var capabilitiesURL = latticeconfig.Env("LATTICE_CONTROL_CAPABILITIES_URL", "http://127.0.0.1:8082/capabilities")

// handleModels serves the client-facing namespace. It reports the capability
// aliases because that is the namespace clients are expected to use, and the
// real context ceiling because a limit the client cannot see is a limit it will
// discover by being truncated.
func handleModels(w http.ResponseWriter, r *http.Request) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(capabilitiesURL)
	if err != nil {
		http.Error(w, "Control plane unavailable", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "Control plane: "+resp.Status, http.StatusServiceUnavailable)
		return
	}

	var caps struct {
		ContextLength int `json:"context_length"`
		Capabilities  []struct {
			ID string `json:"id"`
		} `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		http.Error(w, "Invalid capabilities from control plane", http.StatusInternalServerError)
		return
	}

	data := make([]map[string]interface{}, 0, len(caps.Capabilities))
	for _, c := range caps.Capabilities {
		data = append(data, map[string]interface{}{
			"id":             c.ID,
			"object":         "model",
			"created":        time.Now().Unix(),
			"owned_by":       "lattice",
			"context_length": caps.ContextLength,
		})
	}

	// Explicit: Go sniffs this as text/plain without the header, and strict
	// OpenAI clients reject that.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
```

Register both in `main`:

```go
	http.HandleFunc("/v1/chat/completions", handleChat)
	http.HandleFunc("/v1/models", handleModels)
	http.HandleFunc("/health", handleHealth)
```

Add `"sort"` to control's imports only — the frontend needs no new import.

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./cmd/lattice-control/ ./cmd/lattice-frontend/ -v`
Expected: PASS.

- [ ] **Step 7: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: no output.

- [ ] **Step 8: Commit**

```bash
git add cmd/lattice-frontend/main.go cmd/lattice-control/main.go cmd/lattice-frontend/handler_test.go cmd/lattice-control/route_test.go cmd/lattice-gateway/main.go
git commit -m "Add a discovery surface so the API stops looking absent

A client that probes before it calls got 404 everywhere and concluded the
lattice had no chat endpoint at all. /v1/models and /health fix the
discovery; /capabilities single-sources the context ceiling from the
gateway that actually enforces it."
```

---

### Task 5: Tool calling through the gateway

The frontend now forwards `tools`, but the gateway unmarshals into a typed `Request` with no `Tools` field, so it would drop them again one hop later. This task carries them to Ollama and translates the answer back.

Ollama returns `tool_calls[].function.arguments` as a **JSON object**; OpenAI clients expect a **JSON string**. That conversion is the substance of this task.

**Files:**
- Modify: `cmd/lattice-gateway/main.go`
- Test: `cmd/lattice-gateway/tools_test.go` (create)

**Interfaces:**
- Produces: OpenAI-shaped `message.tool_calls` and `finish_reason: "tool_calls"` on the unary path.

- [ ] **Step 1: Write the failing test**

Create `cmd/lattice-gateway/tools_test.go`:

```go
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Ollama answers a tool call with arguments as a JSON object; OpenAI clients
// expect a JSON string. Translating that, and synthesising the call id Ollama
// does not provide, is what makes an agent's tool loop work at all.
func TestToolCallsAreTranslatedToOpenAIShape(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]interface{}
		json.NewDecoder(r.Body).Decode(&got)
		if _, ok := got["tools"]; !ok {
			t.Error("tools were not forwarded to Ollama")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"hermes3:8b","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Paris"}}}]},"prompt_eval_count":5,"eval_count":7}`))
	}))
	defer ollama.Close()

	p := &OllamaProvider{Endpoint: ollama.URL}
	res, err := p.Execute(Request{
		Model:    "hermes3:8b",
		Messages: []interface{}{map[string]interface{}{"role": "user", "content": "weather?"}},
		Tools:    []interface{}{map[string]interface{}{"type": "function"}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	msg := res.Choices[0].Message
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.Type != "function" {
		t.Errorf("type = %q, want function", tc.Type)
	}
	if tc.Function.Name != "get_weather" {
		t.Errorf("name = %q", tc.Function.Name)
	}
	// The arguments must be a JSON *string*, not an object.
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments are not a JSON string: %q (%v)", tc.Function.Arguments, err)
	}
	if args["city"] != "Paris" {
		t.Errorf("arguments lost the payload: %v", args)
	}
	if tc.ID == "" {
		t.Error("tool call has no id — OpenAI clients key their reply on it")
	}

	if got := res.Choices[0].FinishReason; got != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", got)
	}
}

// A plain answer must not grow a tool_calls field: omitempty, not an empty array.
func TestPlainAnswerOmitsToolCalls(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"hermes3:8b","message":{"role":"assistant","content":"hello"},"prompt_eval_count":1,"eval_count":2}`))
	}))
	defer ollama.Close()

	p := &OllamaProvider{Endpoint: ollama.URL}
	res, err := p.Execute(Request{Model: "hermes3:8b", Messages: []interface{}{}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	b, _ := json.Marshal(res.Choices[0].Message)
	if strings.Contains(string(b), "tool_calls") {
		t.Errorf("a plain answer serialised tool_calls: %s", b)
	}
	if got := res.Choices[0].FinishReason; got != "stop" {
		t.Errorf("finish_reason = %q, want stop", got)
	}
}

// Tool results come back as ordinary messages, so the gateway must not reshape
// them; this pins that a `tool` role message reaches Ollama untouched.
func TestToolMessagesAreForwardedUnchanged(t *testing.T) {
	var seen []interface{}
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]interface{}
		json.NewDecoder(r.Body).Decode(&got)
		seen, _ = got["messages"].([]interface{})
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"hermes3:8b","message":{"role":"assistant","content":"ok"},"prompt_eval_count":1,"eval_count":1}`))
	}))
	defer ollama.Close()

	p := &OllamaProvider{Endpoint: ollama.URL}
	_, err := p.Execute(Request{
		Model: "hermes3:8b",
		Messages: []interface{}{
			map[string]interface{}{"role": "assistant", "content": "", "tool_calls": []interface{}{}},
			map[string]interface{}{"role": "tool", "content": `{"temp":18}`, "tool_call_id": "call_0"},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("Ollama saw %d messages, want 2", len(seen))
	}
	second, _ := seen[1].(map[string]interface{})
	if second["role"] != "tool" || second["tool_call_id"] != "call_0" {
		t.Errorf("tool message was reshaped: %v", second)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/lattice-gateway/ -run 'TestTool|TestPlainAnswer' -v`
Expected: FAIL — `unknown field Tools in struct literal`.

- [ ] **Step 3: Add the types**

In `cmd/lattice-gateway/main.go`, add to the `Request` struct:

```go
type Request struct {
	Model      string        `json:"model"`
	Messages   []interface{} `json:"messages"`
	Stream     bool          `json:"stream"`
	Tools      []interface{} `json:"tools,omitempty"`
	ToolChoice interface{}   `json:"tool_choice,omitempty"`
	Routing    Routing       `json:"routing"`
}
```

Add the OpenAI-shaped tool call, and extend `Message`:

```go
// ToolFunction is named rather than inlined so the response-building code below
// can construct one without restating its tags, which are part of the type.
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall is the OpenAI shape. Ollama returns the same information with
// arguments as a JSON object, which is translated in Execute.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}
```

- [ ] **Step 4: Forward tools in both provider paths**

In `OllamaProvider.Execute`, inside the `ollamaReq` map literal, add after `"messages"`:

```go
		"messages": req.Messages,
		"tools":    req.Tools,
```

and set the key only when present, immediately after the literal:

```go
	if len(req.Tools) == 0 {
		// Ollama rejects tools: null; the key must simply be absent.
		delete(ollamaReq, "tools")
	}
	if req.ToolChoice != nil {
		ollamaReq["tool_choice"] = req.ToolChoice
	}
```

Make the same two additions in `ExecuteStream`.

- [ ] **Step 5: Translate the response**

In `OllamaProvider.Execute`, extend the `native` struct:

```go
	var native struct {
		Model   string `json:"model"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			// Ollama's arguments are an object; OpenAI's are a string. Decoding
			// as RawMessage and re-encoding below performs that translation
			// without guessing at the inner shape.
			ToolCalls []struct {
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		PromptEvalCount int `json:"prompt_eval_count"`
		EvalCount       int `json:"eval_count"`
	}
```

Replace the `Message:` field of the returned `Response`:

```go
		toolCalls := make([]ToolCall, 0, len(native.Message.ToolCalls))
		for i, tc := range native.Message.ToolCalls {
			args := string(tc.Function.Arguments)
			if args == "" || args == "null" {
				args = "{}"
			}
			toolCalls = append(toolCalls, ToolCall{
				// Ollama issues no call id. OpenAI clients key the tool result on
				// one, so it is synthesized deterministically per position.
				ID:       fmt.Sprintf("call_%d", i),
				Type:     "function",
				Function: ToolFunction{Name: tc.Function.Name, Arguments: args},
			})
		}

		finish := "stop"
		if len(toolCalls) > 0 {
			// The client's tool loop branches on this; "stop" with tool_calls
			// present makes an agent end its turn instead of calling the tool.
			finish = "tool_calls"
		}

		return &Response{
			ID:      "chatcmpl-" + req.Routing.RequestID,
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   native.Model,
			Choices: []Choice{{
				Index:        0,
				Message:      Message{Role: native.Message.Role, Content: native.Message.Content, ToolCalls: toolCalls},
				FinishReason: finish,
			}},
			Usage: Usage{
				PromptTokens:     native.PromptEvalCount,
				CompletionTokens: native.EvalCount,
				TotalTokens:      native.PromptEvalCount + native.EvalCount,
			},
		}, nil
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./cmd/lattice-gateway/ -v`
Expected: PASS.

- [ ] **Step 7: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: no output.

- [ ] **Step 8: Commit**

```bash
git add cmd/lattice-gateway/main.go cmd/lattice-gateway/tools_test.go
git commit -m "Gateway: carry tool calls through to Ollama and back

The gateway decoded into a struct with no tools field, so Go dropped them
silently one hop after the frontend stopped doing the same. The arguments
also need translating: Ollama returns an object where OpenAI clients expect
a JSON string, and Ollama issues no call id at all.

Streaming tool calls are not handled yet -- only the unary path."
```

---

### Task 6: Raise the context ceiling and measure it

**This is the one task whose acceptance is a measurement, not a test.** A larger ceiling means a larger KV cache, and the Mac's SSD is what the memory margin exists to protect.

**Files:**
- Modify: `cmd/lattice-gateway/main.go:469`
- Modify: `deploy/` env templates (the gateway's `LATTICE_GATEWAY_MAX_CONTEXT`, if pinned there)

**Interfaces:**
- Consumes: the `/health` payload from Task 4, which advertises this value.

- [ ] **Step 1: Change the default**

In `cmd/lattice-gateway/main.go`, in `maxContextTokens`, change the fallback and its comment:

```go
func maxContextTokens() int {
	if v := latticeconfig.Env("LATTICE_GATEWAY_MAX_CONTEXT", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	// 65536 matches what agent runtimes carry by default. It is a memory
	// decision as much as a formatting one: a larger ceiling is a larger KV
	// cache, so this value is only trusted once the swap measurement below
	// says it is affordable.
	return 65536
}
```

Also update the doc comment above `var maxContext` — it currently says the default is 32768.

- [ ] **Step 2: Run the suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS. No test asserts the old default.

- [ ] **Step 3: Check the deploy templates**

Run: `grep -rn "MAX_CONTEXT" deploy/ docs/ 2>/dev/null`
Expected: any pinned value is updated to `65536`, or the key is left unset so the new default applies. The operations manual's variable table (§3) must state `65536`.

- [ ] **Step 4: Build, deploy, and verify it is advertised**

```bash
GOOS=darwin GOARCH=arm64 go build -o bin/lattice-gateway ./cmd/lattice-gateway
./deploy/install-macos.sh
curl -s http://localhost:8081/health
```

Expected: `{"status":"ok","max_context":65536}`.

Then confirm the ceiling propagated through control:

```bash
curl -s http://127.0.0.1:8082/capabilities
```

Expected: `"context_length":65536` (allow up to 10s for the health loop).

- [ ] **Step 5: Measure swap — the acceptance test**

Before a warm local request, note the counter:

```bash
vm_stat | grep -i swap
```

Run one large-context local request through the frontend, then read it again:

```bash
vm_stat | grep -i swap
```

Expected: **`Swapouts` is flat across the request.** If it is not, the ceiling is wrong — revert it to `32768` and record the measurement in the spec's §3.5. This outranks the convenience that motivated the change.

- [ ] **Step 6: Commit**

```bash
git add cmd/lattice-gateway/main.go deploy/ docs/
git commit -m "Raise the gateway context ceiling to 65536"

# If Step 5 showed swap growth, the commit instead reverts and records why:
# git commit -m "Keep the gateway context ceiling at 32768: 65536 pushed the Mac into swap"
```

---

### Task 7: Bring the documents back into conformance

The specs currently describe behaviour the binaries did not have, and one promise the policy table never kept.

**Files:**
- Modify: `docs/specs/lattice-frontend.md` §2
- Modify: `docs/specs/lattice-control.md`
- Modify: `docs/specs/lattice-gateway.md`
- Modify: `docs/specs/inference-v1.md` §2
- Modify: `docs/handbook/architecture-handbook.md` §11
- Modify: `docs/guide/user-guide.md`
- Modify: `docs/manual/operations-manual.md` §3
- Modify: `docs/README.md` (register the new spec)

- [ ] **Step 1: Amend the frontend spec**

In `docs/specs/lattice-frontend.md` §2, state the passthrough rule and the two discovery endpoints explicitly — including the rule that the frontend may not enumerate fields, since that is the invariant the bug violated.

- [ ] **Step 2: Amend the control spec**

In `docs/specs/lattice-control.md`, document `resolveModel`'s two modes, the "`model_name` is never empty" rule, `GET /capabilities`, and the stored `gatewayMaxContext`.

- [ ] **Step 3: Amend the gateway spec**

In `docs/specs/lattice-gateway.md`, document that `tools` are forwarded, that Ollama's object-typed `arguments` are translated to an OpenAI string, that call ids are synthesized, that `finish_reason` becomes `tool_calls`, and that the `/health` payload carries `max_context`. Note the streaming limitation.

- [ ] **Step 4: Record the `LOCAL_PREFERRED` drift**

In `docs/specs/inference-v1.md` §2, mark `LOCAL_PREFERRED` as **not implemented** — the policy table routes only `LOCAL_ONLY` to local-with-no-fallback and everything else by latency class. Do not implement it here; make the drift visible so it is not rediscovered.

- [ ] **Step 5: Add the gotcha**

In `docs/handbook/architecture-handbook.md` §11, add:

> **A struct decode drops fields as silently as a whitelist does.** The frontend
> rebuilt its proxy body from named fields and lost `tools`; the gateway then
> decoded into a struct with no `tools` field and lost them again, one hop
> later — and nothing in either handler mentioned the field, so neither looked
> wrong. A whitelist at least names what it discards. When proxying, forward the
> caller's body and rewrite only what routing requires.

- [ ] **Step 6: Update the user-facing docs**

- `docs/guide/user-guide.md`: document a literal model name as an alternative to a capability alias, and note that `X-Request-Id` comes back on every response.
- `docs/manual/operations-manual.md` §3: update `LATTICE_GATEWAY_MAX_CONTEXT` to `65536`; add `LATTICE_CONTROL_CAPABILITIES_URL` to the frontend table; add a troubleshooting row for "an agent's tool call comes back with empty content" → check `finish_reason` and the gateway's tool translation.
- `docs/README.md`: add `lattice-agent-surface.md` to the spec index.

- [ ] **Step 7: Verify no addresses or account names were introduced**

Run: `grep -rniE "tailnet|100\.|192\.168\.|@rpi|@mac" docs/ deploy/ 2>/dev/null`
Expected: no matches outside placeholders. Invariant 8.

- [ ] **Step 8: Commit**

```bash
git add docs/
git commit -m "Document the agent-facing surface and the LOCAL_PREFERRED drift"
```

---

### Task 8: Reconcile with the parallel shim

An external agent platform is independently specifying a translation shim for Lattice, on the premise — established as false — that Lattice has no chat endpoint. It was deliberately allowed to land rather than blocked.

**Deliverable:** a written reconciliation, not code. Do not adopt the shim.

- [ ] **Step 1: Find what landed**

```bash
ls -la /Users/archcore/Projects/golem/dsh-handoff/
find /Users/archcore/Projects/golem -iname "*shim*" -o -iname "*lattice*" | head -20
```

- [ ] **Step 2: Diff its spec against ours**

Read whatever shim brief or code landed and compare field by field against `docs/specs/lattice-agent-surface.md`. Look specifically for: body fields it forwards that this plan does not, error-status choices, and any client-side configuration it documents.

- [ ] **Step 3: Fold in what it got right, in writing**

Append a short subsection to `docs/specs/lattice-agent-surface.md` §7 recording what the shim assumed, what this implementation does instead, and any idea worth adopting. If it discovered a gap this plan missed, add a task for it rather than quietly widening an existing one.

- [ ] **Step 4: Commit**

```bash
git add docs/specs/lattice-agent-surface.md
git commit -m "Record the reconciliation with the parallel shim"
```

---

## Self-Review

**Spec coverage:**

| spec requirement | task |
|---|---|
| §3.1 body passthrough | 1 |
| §3.2 literal passthrough, never-empty guard | 3 |
| §3.3 `/v1/models` and `/health` | 4 |
| §3.4 `X-Request-Id`, ordering constraint | 2 |
| §3.5 context ceiling, advertised + measured | 4 (advertise), 6 (raise + measure) |
| §4 non-goals | honoured throughout; `LOCAL_PREFERRED` recorded in 7 |
| §5 invariants | Global Constraints; verified in 7 step 7 |
| §6 acceptance 1–5 | tests in tasks 1, 2, 3, 4 |
| §6 acceptance 6 (tool calling e2e) | 5 |
| §6 acceptance 7 (swap measurement) | 6 step 5 |
| §7 reconciliation | 8 |

**Two gaps found while writing this and folded in rather than deferred:** the gateway also drops `tools` (Task 5), and the context ceiling had no single source, so advertising it required the gateway to report it (Task 4).

**Type consistency checked:** `buildProxyBody(raw, decision, requestID, providerParams)` is used with the same four arguments in Task 1's tests and Task 2's integration test; `resolveModel(model, target)` takes `"local"`/`"cloud"` in both the tests and `handleRoute`; `gatewayMaxContext` is declared in Task 4 and read in the same task's `/capabilities` test; `ToolCall.Function.Arguments` is a `string` on the way out in both the type definition and the test's `json.Unmarshal([]byte(...))`.

**Deliberately out of scope, and named so it is not mistaken for an oversight:** streaming tool calls (Task 5, unary only), `/v1/embeddings`, `LOCAL_PREFERRED`, and a model inventory in control.

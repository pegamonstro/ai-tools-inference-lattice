# Gateway Registry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make gateways announce their models, capabilities, and slots; make the control plane build one registry from those announcements; and route to an adequate gateway by filter then score.

**Architecture:** The gateway flattens its provider registry into a `/health` announcement (`capabilities`, `slots`, `models`). The control plane collapses its two hardcoded registries (`providers`, `gateways`) into one `Gateway` type — cloud entries declared in config, local entries populated from the announcement — and replaces the two-branch routing with one `selectGateway` filter → score function.

**Tech Stack:** Go stdlib only. No new dependencies, no new infrastructure. Discovery is the existing 10 s `/health` poll.

**Spec:** `docs/specs/lattice-gateway-registry.md` — the plan argues from the spec, so the spec travels with it; executors read both.

## Global Constraints

- Go stdlib only; no new dependencies.
- No new infrastructure (no Redis/Kafka/DB); discovery via HTTP polling, no registration protocol.
- `LOCAL_ONLY` is never silently promoted to cloud; a request no gateway can serve fails loudly (`StatusConflict` / `ServiceUnavailable`).
- The Mac is the only host that computes local models; rpi4/rpi3 are cloud-only.
- Commit style: imperative mood, sentence case, no `feat:`/`fix:` prefix, trailer `Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>`.
- No usernames, hostnames, or addresses in tracked files.

---

## Task 1: Gateway announces capabilities, slots, and models

The gateway's `/health` gains three fields and drops `providers`. This is deployable on its own: the old control plane reads `providers: null` and ignores the new fields.

**Files:**
- Modify: `cmd/lattice-gateway/providers.go` (registry struct, two announce helpers, tag filter)
- Modify: `cmd/lattice-gateway/main.go:917-950` (`handleHealth`)
- Test: `cmd/lattice-gateway/providers_test.go` (replace the `capabilityAnnouncement` assertion, add three tests)

**Interfaces:**
- Produces (gateway, for later tasks): `/health` JSON now `{"status","max_context","capabilities":[]string,"slots":int,"models":[]string}`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/lattice-gateway/providers_test.go`, replacing `TestLoadProvidersBuildsRegistryAndAnnouncement`'s `capabilityAnnouncement` assertion with the new helpers, and adding:

```go
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestAnnouncedCapabilitiesChatOnlyForMLX(t *testing.T) {
	reg := &providerRegistry{providerKinds: map[string]string{"mlx": "mlx"}}
	caps := reg.announcedCapabilities()
	if contains(caps, "embeddings") {
		t.Fatalf("MLX-only gateway must not announce embeddings: %v", caps)
	}
}

func TestAnnouncedCapabilitiesAddsOllamaModalities(t *testing.T) {
	reg := &providerRegistry{providerKinds: map[string]string{"ollama": "ollama", "mlx": "mlx"}}
	caps := reg.announcedCapabilities()
	for _, want := range []string{"local", "chat", "embeddings", "tool_calling"} {
		if !contains(caps, want) {
			t.Fatalf("announcedCapabilities missing %q: %v", want, caps)
		}
	}
}

func TestAnnouncedModelsFlattensAndDropsCloudTags(t *testing.T) {
	reg := &providerRegistry{
		served: map[string][]string{"mlx": {"qwen2.5-coder:3b"}, "ollama": nil},
	}
	models := reg.announcedModels([]string{"granite3-moe:3b", "gemma4:31b-cloud", "qwen2.5-coder:3b"})
	want := map[string]bool{"granite3-moe:3b": true, "qwen2.5-coder:3b": true}
	if len(models) != len(want) {
		t.Fatalf("got %v, want exactly %v", models, want)
	}
	for _, m := range models {
		if !want[m] {
			t.Fatalf("unexpected model %q (or cloud tag not dropped)", m)
		}
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./cmd/lattice-gateway/ -run 'Announced|LoadProviders'`
Expected: FAIL — `providerRegistry has no field providerKinds`, `announcedCapabilities` / `announcedModels` undefined, and the old test still references `capabilityAnnouncement`.

- [ ] **Step 3: Implement**

In `cmd/lattice-gateway/providers.go`, add `providerKinds map[string]string` to the `providerRegistry` struct (after `served`), initialise it in `loadProviders`:

```go
reg := &providerRegistry{
	defaultProvider: "ollama",
	modelProviders:  map[string]string{},
	modelUpstream:   map[string]string{},
	served:          map[string][]string{},
	providerKinds:   map[string]string{},
}
```

In the `cfgPath == ""` legacy path, after `provs["ollama"] = ...`, add `reg.providerKinds["ollama"] = "ollama"`. In the providers loop, after `reg.served[p.Name] = nil`, add `reg.providerKinds[p.Name] = p.Kind`.

Delete `capabilityAnnouncement()` (lines 133–142) and replace with:

```go
// announcedCapabilities is the capability half of the /health announcement:
// what this gateway can serve. "local" and "chat" are intrinsic to the host;
// "embeddings" and "tool_calling" come from Ollama (MLX is chat-only).
func (reg *providerRegistry) announcedCapabilities() []string {
	caps := []string{"local", "chat"}
	for _, kind := range reg.providerKinds {
		if kind == "ollama" {
			caps = append(caps, "embeddings", "tool_calling")
			break
		}
	}
	return caps
}

// isCloudTag reports whether a model name is an Ollama routing alias rather than
// a local model — the ":cloud" / "<size>-cloud" suffix the measurements record
// documents.
func isCloudTag(name string) bool {
	i := strings.LastIndex(name, ":")
	if i < 0 {
		return false
	}
	tag := name[i+1:]
	return tag == "cloud" || strings.HasSuffix(tag, "-cloud")
}

// announcedModels is the flattened list of local model ids the gateway serves,
// across all providers: every model explicitly routed in config, plus every
// local model Ollama reports via /api/tags, cloud aliases dropped.
func (reg *providerRegistry) announcedModels(ollamaTags []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, models := range reg.served {
		for _, m := range models {
			if m != "" && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	for _, m := range ollamaTags {
		if m != "" && !isCloudTag(m) && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}
```

In `cmd/lattice-gateway/main.go`, rewrite `handleHealth` (lines 917–950) so it decodes `/api/tags` and emits the new shape:

```go
func handleHealth(w http.ResponseWriter, r *http.Request) {
	if !budgeter.CanAccommodate() {
		http.Error(w, "Memory pressure high", http.StatusServiceUnavailable)
		return
	}

	resp, err := http.Get(ollamaURL + "/api/tags")
	if err != nil {
		http.Error(w, "Ollama unhealthy", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "Ollama unhealthy", http.StatusServiceUnavailable)
		return
	}

	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tags)
	tagNames := make([]string, 0, len(tags.Models))
	for _, m := range tags.Models {
		tagNames = append(tagNames, m.Name)
	}

	caps := []string{"local", "chat", "embeddings", "tool_calling"}
	models := []string{}
	if registry != nil {
		caps = registry.announcedCapabilities()
		models = registry.announcedModels(tagNames)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":       "ok",
		"max_context":  maxContext,
		"capabilities": caps,
		"slots":        cap(inferenceSlots),
		"models":       models,
	})
}
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./cmd/lattice-gateway/`
Expected: PASS.

- [ ] **Step 5: Build, vet, commit**

```bash
go build ./... && go vet ./... && go test ./cmd/lattice-gateway/
git add cmd/lattice-gateway/providers.go cmd/lattice-gateway/main.go cmd/lattice-gateway/providers_test.go
git commit -m "Announce capabilities, slots, and models from the gateway health endpoint"
```

---

## Task 2: Control — unify the registry into one announced Gateway type

Collapse `Provider` + `Gateway` into one `Gateway` type and one mutex-guarded registry; wire the health poll to populate the local entry from the announcement. Behavior-preserving: routing still selects "cheapest cloud / first healthy local", now reading the unified map.

**Files:**
- Modify: `cmd/lattice-control/main.go` (structs at 54–66, `init()` 81–105, `localityFor` 113–121, `monitorHealth` 155–187, the routing branch 644–690, `handleCapabilities` 565–592, the `gatewayMaxContext`/`gatewayProviders` vars 199–208)
- Test: `cmd/lattice-control/main_test.go` (Create — `decodeHealth`)

**Interfaces:**
- Consumes: the gateway `/health` JSON from Task 1.
- Produces: `type Gateway struct{ID, Endpoint string; Capabilities, Models []string; Slots, RateLimit int; CostPerToken float64; MaxContext int}`; `func decodeHealth(body []byte) (gatewayAnnouncement, error)`; `func hasCapability(caps []string, want string) bool`.

- [ ] **Step 1: Write the failing test**

Create `cmd/lattice-control/main_test.go`:

```go
package main

import "testing"

func TestDecodeHealth(t *testing.T) {
	body := []byte(`{"status":"ok","max_context":32768,"capabilities":["local","chat"],"slots":1,"models":["granite3-moe:3b"]}`)
	a, err := decodeHealth(body)
	if err != nil {
		t.Fatalf("decodeHealth: %v", err)
	}
	if a.MaxContext != 32768 || a.Slots != 1 {
		t.Fatalf("got max_context=%d slots=%d", a.MaxContext, a.Slots)
	}
	if !hasCapability(a.Capabilities, "local") {
		t.Fatalf("capabilities %v missing local", a.Capabilities)
	}
	if len(a.Models) != 1 || a.Models[0] != "granite3-moe:3b" {
		t.Fatalf("models = %v", a.Models)
	}
}

func TestDecodeHealthMissingFieldsZero(t *testing.T) {
	a, err := decodeHealth([]byte(`{"status":"ok"}`))
	if err != nil {
		t.Fatalf("decodeHealth: %v", err)
	}
	if a.MaxContext != 0 || a.Slots != 0 || a.Capabilities != nil || a.Models != nil {
		t.Fatalf("expected zero values, got %+v", a)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/lattice-control/ -run TestDecodeHealth`
Expected: FAIL — `decodeHealth` undefined.

- [ ] **Step 3: Implement the types and decode**

Replace the `Provider` (54–60) and `Gateway` (62–66) structs with one type, and add the announcement + decode:

```go
type Gateway struct {
	ID           string
	Endpoint     string
	Capabilities []string // locality ("local","tiny","cloud") + modality
	Models       []string // concrete models served; empty = wildcard (cloud)
	Slots        int      // local concurrency ceiling (0 = not slot-limited)
	CostPerToken float64  // cloud only
	RateLimit    int      // cloud only, req/min
	MaxContext   int      // announced context ceiling
}

// gatewayAnnouncement is the /health payload the gateway publishes.
type gatewayAnnouncement struct {
	MaxContext   int      `json:"max_context"`
	Capabilities []string `json:"capabilities"`
	Slots        int      `json:"slots"`
	Models       []string `json:"models"`
}

func decodeHealth(body []byte) (gatewayAnnouncement, error) {
	var a gatewayAnnouncement
	err := json.Unmarshal(body, &a)
	return a, err
}

func hasCapability(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Unify the registry**

Replace the two-var block (75–79) with:

```go
var (
	// gateway registry: every inference target, cloud and local, as one type.
	// Cloud entries are declared; local entries have Models/Slots/MaxContext
	// filled from the gateway's /health announcement.
	gateways = map[string]Gateway{}
)
```

Rewrite `init()` (81–105):

```go
func init() {
	ollamaURL := latticeconfig.Env("LATTICE_OLLAMA_URL", "http://localhost:11434")
	gatewayURL := latticeconfig.Env("LATTICE_GATEWAY_URL", "http://localhost:8081")

	gateways["ollama-cloud-primary"] = Gateway{
		ID:           "ollama-cloud-primary",
		Endpoint:     ollamaURL,
		Capabilities: []string{"cloud"},
		CostPerToken: 0.00001,
		RateLimit:    100,
	}
	gateways["ollama-cloud-secondary"] = Gateway{
		ID:           "ollama-cloud-secondary",
		Endpoint:     ollamaURL, // Same endpoint, different account/key
		Capabilities: []string{"cloud"},
		CostPerToken: 0.000005,
		RateLimit:    10,
	}
	gateways["mac-gateway"] = Gateway{
		ID:           "mac-gateway",
		Endpoint:     gatewayURL,
		Capabilities: []string{"local"},
	}
}
```

Rewrite `localityFor` (113–121):

```go
func localityFor(targetID string) string {
	healthMutex.RLock()
	gw, ok := gateways[targetID]
	healthMutex.RUnlock()
	if !ok {
		return "unknown"
	}
	if hasCapability(gw.Capabilities, "cloud") {
		return "cloud"
	}
	return "local"
}
```

Rewrite `monitorHealth` (155–187) to poll only local gateways and merge the announcement:

```go
func monitorHealth() {
	for {
		for id, gw := range gateways {
			if !hasCapability(gw.Capabilities, "local") && !hasCapability(gw.Capabilities, "tiny") {
				continue // declared cloud: no /health, no telemetry relay
			}
			resp, err := http.Get(gw.Endpoint + "/health")
			healthy := err == nil && resp != nil &&
				(resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound)
			if healthy && resp != nil && resp.StatusCode == http.StatusOK {
				body, rerr := io.ReadAll(resp.Body)
				if rerr == nil {
					if a, derr := decodeHealth(body); derr == nil {
						healthMutex.Lock()
						g2 := gateways[id]
						g2.Models = a.Models
						g2.Slots = a.Slots
						g2.MaxContext = a.MaxContext
						if len(a.Capabilities) > 0 {
							g2.Capabilities = a.Capabilities
						}
						gateways[id] = g2
						healthMutex.Unlock()
					}
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
		time.Sleep(10 * time.Second)
	}
}
```

Delete the `gatewayMaxContext` / `gatewayProviders` vars (199–208) — they are folded into `gateways`.

- [ ] **Step 5: Adapt the routing branch and capabilities handler**

Replace the two-branch selection in `handleRoute` (641–690) with an equivalent read of the unified map (semantics preserved — cheapest cloud, first healthy local; `selectGateway` arrives in Task 3):

```go
	var targetID, endpoint, modelName string

	healthMutex.RLock()
	healthy := make(map[string]bool, len(gatewayHealthy))
	for k, v := range gatewayHealthy {
		healthy[k] = v
	}
	healthMutex.RUnlock()

	if requiredCap == "cloud" {
		bestCost := math.MaxFloat64
		for id, gw := range gateways {
			if hasCapability(gw.Capabilities, "cloud") && gw.CostPerToken < bestCost {
				bestCost = gw.CostPerToken
				targetID = id
				endpoint = gw.Endpoint
			}
		}
		modelName = resolveModel(req.Model, "cloud")
	} else {
		for id, gw := range gateways {
			if hasCapability(gw.Capabilities, "local") && healthy[id] {
				targetID = id
				endpoint = gw.Endpoint
				modelName = resolveModel(req.Model, "local")
				break
			}
		}
		if targetID == "" {
			tele.Error = "No healthy local gateway found"
			http.Error(w, "No healthy local gateway found", http.StatusServiceUnavailable)
			return
		}
	}
```

(Note: the map iteration order is non-deterministic; for the single local gateway and the explicit cheapest-cloud scan this is fine. `selectGateway` in Task 3 makes scoring explicit.)

Update `handleCapabilities` (565–592) to read the gateway from the registry instead of the deleted singletons:

```go
func handleCapabilities(w http.ResponseWriter, r *http.Request) {
	healthMutex.RLock()
	ctxCap := 0
	var models []string
	var caps []string
	slots := 0
	if gw, ok := gateways["mac-gateway"]; ok {
		ctxCap = gw.MaxContext
		models = gw.Models
		caps = gw.Capabilities
		slots = gw.Slots
	}
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
		"gateway": map[string]interface{}{
			"capabilities": caps,
			"slots":        slots,
			"models":       models,
		},
	})
}
```

- [ ] **Step 6: Run tests, build, vet, commit**

```bash
go test ./cmd/lattice-control/ && go build ./... && go vet ./...
git add cmd/lattice-control/main.go cmd/lattice-control/main_test.go
git commit -m "Unify the control registry into one announced gateway type"
```

---

## Task 3: Control — adequacy routing (filter → score)

Extract the selection into a pure `selectGateway` that filters on health, capability, model-hosting, and slot, then scores cloud by cost. Swap it into `handleRoute`.

**Files:**
- Modify: `cmd/lattice-control/main.go` (`selectGateway`, `hostsModel`, and the `handleRoute` selection)
- Test: `cmd/lattice-control/main_test.go` (table-driven `selectGateway`)

**Interfaces:**
- Consumes: `Gateway`, `hasCapability` (Task 2).
- Produces: `func hostsModel(models []string, model string) bool`; `func selectGateway(requiredCap, model string, registry map[string]Gateway, healthy map[string]bool) (Gateway, error)`.

- [ ] **Step 1: Write the failing test**

Append to `cmd/lattice-control/main_test.go`:

```go
func gw(id string, caps []string, models []string, slots int, cost float64) Gateway {
	return Gateway{ID: id, Capabilities: caps, Models: models, Slots: slots, CostPerToken: cost}
}

func TestSelectGatewayLocalFiltersOnHostingAndSlot(t *testing.T) {
	reg := map[string]Gateway{
		"mac": gw("mac", []string{"local", "chat"}, []string{"granite3-moe:3b"}, 1, 0),
	}
	// hosts the model, has a slot, healthy
	_, err := selectGateway("local", "granite3-moe:3b", reg, map[string]bool{"mac": true})
	if err != nil {
		t.Fatalf("expected match, got %v", err)
	}
	// does not host the model
	if _, err := selectGateway("local", "qwen2.5-coder:7b", reg, map[string]bool{"mac": true}); err == nil {
		t.Fatal("expected no match for unhosted model")
	}
	// unhealthy
	if _, err := selectGateway("local", "granite3-moe:3b", reg, map[string]bool{"mac": false}); err == nil {
		t.Fatal("expected no match for unhealthy gateway")
	}
	// zero slots
	reg["mac"] = gw("mac", []string{"local", "chat"}, []string{"granite3-moe:3b"}, 0, 0)
	if _, err := selectGateway("local", "granite3-moe:3b", reg, map[string]bool{"mac": true}); err == nil {
		t.Fatal("expected no match for zero-slot gateway")
	}
}

func TestSelectGatewayCloudPicksCheapest(t *testing.T) {
	reg := map[string]Gateway{
		"primary":   gw("primary", []string{"cloud"}, nil, 0, 0.00001),
		"secondary": gw("secondary", []string{"cloud"}, nil, 0, 0.000005),
	}
	g, err := selectGateway("cloud", "gemma4:31b-cloud", reg, nil)
	if err != nil {
		t.Fatalf("expected match, got %v", err)
	}
	if g.ID != "secondary" {
		t.Fatalf("expected cheapest (secondary), got %s", g.ID)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/lattice-control/ -run TestSelectGateway`
Expected: FAIL — `selectGateway` and `hostsModel` undefined.

- [ ] **Step 3: Implement**

Add to `cmd/lattice-control/main.go` (near `hasCapability`):

```go
// hostsModel reports whether a gateway serves a concrete model. An empty Models
// list is the cloud wildcard: a subscription hosts any ":cloud" model.
func hostsModel(models []string, model string) bool {
	if len(models) == 0 {
		return true
	}
	for _, m := range models {
		if m == model {
			return true
		}
	}
	return false
}

// selectGateway is the adequacy routing rule: filter on health (announced
// gateways only), capability, model-hosting, and slot; then score cloud by
// lowest cost and local by first match. It returns an error when nothing is
// adequate, so the caller fails loudly rather than reroutes.
func selectGateway(requiredCap, model string, registry map[string]Gateway, healthy map[string]bool) (Gateway, error) {
	var best Gateway
	bestCost := math.MaxFloat64
	found := false

	for _, gw := range registry {
		if !hasCapability(gw.Capabilities, requiredCap) {
			continue
		}
		isLocal := hasCapability(gw.Capabilities, "local") || hasCapability(gw.Capabilities, "tiny")
		if isLocal && !healthy[gw.ID] {
			continue
		}
		if !hostsModel(gw.Models, model) {
			continue
		}
		if isLocal && gw.Slots == 0 {
			continue
		}
		if requiredCap == "cloud" {
			if gw.CostPerToken < bestCost {
				bestCost = gw.CostPerToken
				best = gw
				found = true
			}
		} else {
			return gw, nil
		}
	}
	if !found {
		return Gateway{}, fmt.Errorf("no adequate gateway for capability %q model %q", requiredCap, model)
	}
	return best, nil
}
```

- [ ] **Step 4: Wire into handleRoute**

Replace the Task 2 selection block in `handleRoute` with a snapshot + `selectGateway`:

```go
	healthMutex.RLock()
	registry := make(map[string]Gateway, len(gateways))
	for k, v := range gateways {
		registry[k] = v
	}
	healthy := make(map[string]bool, len(gatewayHealthy))
	for k, v := range gatewayHealthy {
		healthy[k] = v
	}
	healthMutex.RUnlock()

	var targetID, endpoint, modelName string

	concrete := resolveModel(req.Model, requiredCap)
	gw, err := selectGateway(requiredCap, concrete, registry, healthy)
	if err != nil {
		tele.Error = err.Error()
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	targetID = gw.ID
	endpoint = gw.Endpoint
	modelName = concrete
```

Remove the now-unused `targetID == ""` guard (696–697) if it becomes dead — `selectGateway` returns an error instead; if still reachable, leave the guard. Keep `tele.Target = targetID`, `tele.Model = modelName`, and the `Decision{...}` construction unchanged.

- [ ] **Step 5: Run tests, build, vet, commit**

```bash
go test ./cmd/lattice-control/ && go build ./... && go vet ./...
git add cmd/lattice-control/main.go cmd/lattice-control/main_test.go
git commit -m "Route to an adequate gateway by filter then score"
```

---

## Task 4: Telemetry records capability and slot inputs

Extend the routing telemetry line so a refusal names *why* — the required capability and whether the model was hosted — not just "no target".

**Files:**
- Modify: `cmd/lattice-control/main.go` (`Telemetry` struct 43–52, the `handleRoute` defer 604–614, and the failure sites)

**Interfaces:**
- Consumes: the `selectGateway` error path from Task 3.

- [ ] **Step 1: Add the fields**

Extend the `Telemetry` struct:

```go
type Telemetry struct {
	RequestID    string  `json:"request_id"`
	DecisionTime float64 `json:"decision_time_s"`
	Target       string  `json:"target"`
	Model        string  `json:"model"`
	Privacy      string  `json:"privacy"`
	LatencyClass string  `json:"latency_class"`
	Locality     string  `json:"locality"`
	RequiredCap  string  `json:"required_cap,omitempty"`
	Error        string  `json:"error,omitempty"`
}
```

In the `handleRoute` defer, add `tele.RequiredCap = requiredCap` — but `requiredCap` is declared later in the function body, so capture it in the defer via the existing pattern: set `tele.RequiredCap` where `requiredCap` is computed (after line 634), and add a field default in the defer is unnecessary. Set it right after `requiredCap, err := requiredCapability(...)`:

```go
	tele.RequiredCap = requiredCap
```

- [ ] **Step 2: Build, vet, commit**

```bash
go build ./... && go vet ./... && go test ./cmd/lattice-control/
git add cmd/lattice-control/main.go
git commit -m "Record the required capability in routing telemetry"
```

---

## Task 5: Docs, build, deploy, verify end-to-end

Bring the component specs and the operations manual in line, then build both binaries and verify the announcement and routing on the live hosts.

**Files:**
- Modify: `docs/specs/lattice-gateway.md` (the `/health` shape and the "announces providers" note)
- Modify: `docs/specs/lattice-control.md` (the routing algorithm: filter → score, unified registry)
- Modify: `docs/manual/operations-manual.md` (the `/capabilities` response shape — `gateway_providers` becomes `gateway`)

- [ ] **Step 1: Amend the docs**

- `lattice-gateway.md`: replace the `providers` field description with `capabilities`, `slots`, and `models`; note the gateway enumerates its local models from Ollama `/api/tags` and drops `:cloud` aliases.
- `lattice-control.md`: replace the two-branch routing description with the unified registry and `selectGateway` filter → score; note the control plane returns a `Decision` and the frontend proxies.
- `operations-manual.md`: update the `LATTICE_GATEWAY_PROVIDERS` row's cross-reference and the `/capabilities` example from `gateway_providers` to `gateway`.

- [ ] **Step 2: Build and cross-compile both binaries**

```bash
go build -o lattice-control ./cmd/lattice-control
go build -o lattice-gateway ./cmd/lattice-gateway
GOOS=linux GOARCH=arm64 go build -o lattice-control-linux-arm64 ./cmd/lattice-control
```

- [ ] **Step 3: Deploy and verify**

Deploy the gateway binary to the Mac and the control binary to the RPi4 following the existing runbook (the LaunchAgent on the Mac, the systemd unit on the Pi — see the operations manual). Then:

```bash
curl -s http://<gateway>:8081/health
# expect: {"status":"ok","max_context":32768,"capabilities":[...],"slots":1,"models":[...]}
curl -s http://<control>:8082/capabilities
# expect: context_length, capabilities (aliases), and gateway{capabilities,slots,models}
```

Send one `LOCAL_ONLY` chat request through the frontend and confirm it routes to `mac-gateway`; send one cloud-tagged request and confirm it routes to `ollama-cloud-secondary` (the cheaper of the two).

- [ ] **Step 4: Commit the docs**

```bash
git add docs/specs/lattice-gateway.md docs/specs/lattice-control.md docs/manual/operations-manual.md
git commit -m "Document the gateway registry and adequacy routing"
```

---

## Self-review

- **Spec coverage:** Task 1 → spec §3.2 (announcement); Task 2 → §3.1, §3.3, §3.5 (unified registry, capability vocabulary, two-resolution registry); Task 3 → §3.4, §3.6 (slots, filter → score); Task 4 → §3.6 fail-loudly (refusal telemetry); Task 5 → §3.3/§5 docs + deploy. No spec section is unimplemented.
- **Placeholder scan:** no `TBD`/`TODO`; every step carries code.
- **Type consistency:** `Gateway`, `gatewayAnnouncement`, `decodeHealth`, `hasCapability`, `hostsModel`, `selectGateway` signatures match across tasks; the gateway's announcement fields (`capabilities`, `slots`, `models`, `max_context`) match the control plane's `gatewayAnnouncement` JSON tags.

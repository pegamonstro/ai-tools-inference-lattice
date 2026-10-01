# Image Gateway + MCP Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route image generation through Lattice — gateway `POST /v1/images/generations` + `/v1/images/edits` over an OpenAI Images contract, fronted by the frontend, routed by control (zero change), executed by a new `mflux` provider — and expose it to DSH via a thin stdio MCP server.

**Architecture:** The gateway gains two handlers that mirror `handleEmbeddings` exactly (memory check against a dedicated ~10 GiB image margin → `acquireSlot` → sidecar call → telemetry on every exit path) and a new `mflux` provider kind that points at the already-running mflux sidecar. The frontend gains two one-line routes reusing `proxyInference` unchanged. Control needs no code change: `requiredCapability` returns `local` for the untagged `flux-dev` name, and `selectGateway` matches `mac-gateway` once `/health` announces the model + `image_generation`. A Python stdio MCP server (official `mcp` SDK) fronts the frontend with two tools.

**Tech Stack:** Go stdlib only (gateway/frontend/control binaries — no new Go deps); Python `mcp` SDK for the MCP server (a client adapter, not a Lattice binary); the mflux sidecar is consumed unchanged.

**Spec:** `docs/specs/lattice-image-gateway.md` — the plan argues from the spec, so the spec travels with it; executors read both.

## Global Constraints

- Go stdlib only for the Lattice binaries (gateway/control/frontend); the Python MCP server is a client adapter, not subject to this.
- No new infrastructure (no Redis/Kafka/DB); the MCP server is stdio-spawned — no listener, no supervision.
- `LOCAL_ONLY` never falls back: an untagged image model routes local, never promoted to cloud; an irreconcilable request fails loudly by name.
- The Mac is the only host that computes local models; rpi4/rpi3 are cloud-only. No inference on the Pi — the MCP server is a thin HTTP adapter.
- Protect the Mac's SSD: the gateway's single slot serializes image generation against chat; the sidecar already runs `--low-ram --vae-tiling`.
- No usernames, hostnames, or addresses in tracked files (loopback defaults such as `127.0.0.1:8080` are the existing convention; real host addresses are config-only).
- Telemetry shares one key: the image request's `request_id` rides the `X-Request-Id` header (an image body carries no routing envelope), so all three streams key the same id.
- Commit style: imperative mood, sentence case, no `feat:`/`fix:` prefix, trailer `Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>`.

---

## File Structure

- **`cmd/lattice-gateway/main.go`** — gains `ImageRequest`/`ImageResponse`/`ImageDataItem` types (Task 1); the image handlers, `imageMarginBytes`, `defaultImageModel`, and a `MemoryBudgeter.freeBytes`/`CanAccommodateWith` refactor (Task 2).
- **`cmd/lattice-gateway/providers.go`** — gains `ImageProvider`, `MfluxProvider`, `parseImageSize`, `imageTimeout`, the `case "mflux"` in `loadProviders`, and `image_generation` in `announcedCapabilities` (Task 1).
- **`cmd/lattice-gateway/providers_test.go`** — provider-level tests (Task 1).
- **`cmd/lattice-gateway/image_test.go`** (new) — handler-level tests (Task 2).
- **`cmd/lattice-frontend/main.go`** — gains two one-line image handlers + routes (Task 3).
- **`cmd/lattice-frontend/handler_test.go`** — frontend image test (Task 3).
- **`deploy/gateway-providers.json`** — gains the `mflux` provider + `flux-dev` model route (Task 1).
- **`deploy/dsh/mcp-imagegen/server.py`** (new) — the MCP server (Task 4).
- **`deploy/dsh/mcp-imagegen/test_server.py`** (new) — the MCP server smoke test (Task 4).
- **`deploy/dsh/mcp-imagegen/README.md`** (new) — install/run/wire instructions (Task 4).
- **`deploy/dsh/mcp-imagegen/cordis.patch.yml.example`** (new) — DSH wiring sample (Task 5).

---

## Task 1: Gateway — the `mflux` provider and image types

**Files:**
- Modify: `cmd/lattice-gateway/main.go:80` (insert image types after the `Usage` struct)
- Modify: `cmd/lattice-gateway/providers.go:107` (add `case "mflux"` in `loadProviders`)
- Modify: `cmd/lattice-gateway/providers.go:163-172` (rewrite `announcedCapabilities`)
- Modify: `cmd/lattice-gateway/providers.go` (append `ImageProvider`, `MfluxProvider`, `parseImageSize`, `imageTimeout` at end of file)
- Modify: `deploy/gateway-providers.json` (add the mflux provider + flux-dev route)
- Test: `cmd/lattice-gateway/providers_test.go`

**Interfaces:**
- Consumes: `Request` (main.go:31), `Response` (main.go:40), `Provider` (main.go:253), `latticeconfig.Env` (pkg/latticeconfig), `loadProviders` switch at providers.go:102.
- Produces:
  - `type ImageRequest struct { Model, Prompt string; N int; Size, ResponseFormat, Image string }`
  - `type ImageResponse struct { Created int64; Data []ImageDataItem }`, `type ImageDataItem struct { B64JSON string }`
  - `type ImageProvider interface { GenerateImage(ctx, ImageRequest) (*ImageResponse, error); EditImage(ctx, ImageRequest) (*ImageResponse, error) }`
  - `type MfluxProvider struct { Endpoint string }` — implements both `Provider` and `ImageProvider`.
  - `func imageTimeout() time.Duration` — default 2 h, env `LATTICE_GATEWAY_IMAGE_TIMEOUT`.
  - `func parseImageSize(size string) (int, int, error)` — default 1024×1024.

- [ ] **Step 1: Write the failing tests**

Add to `cmd/lattice-gateway/providers_test.go` (extend the import block with `context`, `encoding/json`, `net/http`, `net/http/httptest`):

```go
func TestLoadProvidersRegistersMfluxAndAnnouncesImageGeneration(t *testing.T) {
	cfg := `{
		"default_provider": "ollama",
		"providers": [
			{"name": "ollama", "kind": "ollama", "endpoint": "http://localhost:11434"},
			{"name": "mflux", "kind": "mflux", "endpoint": "http://127.0.0.1:8899"}
		],
		"models": [
			{"name": "flux-dev", "provider": "mflux"}
		]
	}`
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	if err := os.WriteFile(path, []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}

	reg, provs, err := loadProviders(path)
	if err != nil {
		t.Fatalf("loadProviders: %v", err)
	}
	if _, ok := provs["mflux"].(*MfluxProvider); !ok {
		t.Errorf("mflux provider not registered as *MfluxProvider")
	}
	if !contains(reg.announcedCapabilities(), "image_generation") {
		t.Errorf("announcedCapabilities missing image_generation: %v", reg.announcedCapabilities())
	}
	models := reg.announcedModels(nil)
	if len(models) != 1 || models[0] != "flux-dev" {
		t.Errorf("announcedModels = %v, want [flux-dev]", models)
	}
}

func TestMfluxProviderGeneratesFromTheSidecar(t *testing.T) {
	var sawPath string
	var sawBody map[string]interface{}
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&sawBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image":"aW1hZ2U=","seed":1,"width":512,"height":512,"seconds":1.0}`))
	}))
	defer sidecar.Close()

	p := &MfluxProvider{Endpoint: sidecar.URL}
	res, err := p.GenerateImage(context.Background(), ImageRequest{Prompt: "a cat", Size: "512x512"})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if sawPath != "/generate" {
		t.Errorf("path = %q, want /generate", sawPath)
	}
	if sawBody["prompt"] != "a cat" {
		t.Errorf("prompt = %v, want 'a cat'", sawBody["prompt"])
	}
	if sawBody["width"] != float64(512) || sawBody["height"] != float64(512) {
		t.Errorf("size not translated to width/height: %v", sawBody)
	}
	if len(res.Data) != 1 || res.Data[0].B64JSON != "aW1hZ2U=" {
		t.Errorf("unexpected response: %+v", res)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/lattice-gateway/ -run 'TestLoadProvidersRegistersMflux|TestMfluxProviderGenerates'`
Expected: FAIL — `undefined: MfluxProvider`, `undefined: ImageRequest`.

- [ ] **Step 3: Write the minimal implementation**

In `cmd/lattice-gateway/main.go`, insert after the `Usage` struct (line 80):

```go
// ImageRequest is the OpenAI Images API request. It is a closed shape on
// purpose: the mflux sidecar honours only prompt, size and count, so the
// request carries exactly those and the edit source image. An unsupported
// OpenAI field (quality, style) never reaches a type that would swallow it.
type ImageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
	// Image is the edit source, base64 (data URL or raw). The Mac cannot read
	// the Pi's filesystem, so the bytes must ride the request.
	Image string `json:"image"`
	// Mask is carried only so a request that sends one fails loudly instead of
	// being silently dropped: the mflux sidecar has no mask support.
	Mask string `json:"mask"`
}

type ImageResponse struct {
	Created int64           `json:"created"`
	Data    []ImageDataItem `json:"data"`
}

type ImageDataItem struct {
	B64JSON string `json:"b64_json"`
}
```

In `cmd/lattice-gateway/providers.go`, add `case "mflux"` to the `loadProviders` switch (line 105-107):

```go
		case "mflux":
			provs[p.Name] = &MfluxProvider{Endpoint: p.Endpoint}
```

Rewrite `announcedCapabilities` (line 163-172) — drop the `break` so a registry with both an Ollama and an mflux provider announces both modality sets, deduped:

```go
func (reg *providerRegistry) announcedCapabilities() []string {
	caps := []string{"local", "chat"}
	seen := map[string]bool{}
	add := func(v string) {
		if !seen[v] {
			seen[v] = true
			caps = append(caps, v)
		}
	}
	for _, kind := range reg.providerKinds {
		switch kind {
		case "ollama":
			add("embeddings")
			add("tool_calling")
		case "mflux":
			add("image_generation")
		}
	}
	return caps
}
```

Append to the end of `cmd/lattice-gateway/providers.go`:

```go
// imageTimeout is the gateway→sidecar client timeout for an image call. It is
// separate from ollamaTimeout because a diffusion step is an order of magnitude
// slower than a chat token: the sidecar's own MFLUX_GEN_TIMEOUT is 3600 s, and
// the client must outlast it rather than inherit the 20 m chat budget.
func imageTimeout() time.Duration {
	if v := latticeconfig.Env("LATTICE_GATEWAY_IMAGE_TIMEOUT", ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 2 * time.Hour
}

// ImageProvider is implemented by providers that generate images. handleImage
// type-asserts to it, exactly as handleInference asserts to StreamingProvider:
// a request that names a provider without the capability fails loudly rather
// than being silently proxied to a chat endpoint that does not exist.
type ImageProvider interface {
	GenerateImage(ctx context.Context, req ImageRequest) (*ImageResponse, error)
	EditImage(ctx context.Context, req ImageRequest) (*ImageResponse, error)
}

// MfluxProvider is an HTTP client for the mflux sidecar (deploy/mflux-sidecar.py),
// which serves FLUX over POST /generate and /edit. It is image-only: Execute
// fails loudly, because a chat request naming the image model is a routing fault
// that must not be silently re-homed to Ollama.
type MfluxProvider struct {
	Endpoint string
}

func (p *MfluxProvider) Name() string { return "mflux" }

// Execute exists only to satisfy Provider. A chat request that names the image
// model reaches here and must fail loudly rather than be silently dropped.
func (p *MfluxProvider) Execute(ctx context.Context, req Request) (*Response, error) {
	return nil, fmt.Errorf("mflux is an image provider, not a chat provider")
}

func (p *MfluxProvider) GenerateImage(ctx context.Context, req ImageRequest) (*ImageResponse, error) {
	return p.runImage(ctx, req, "generate")
}

func (p *MfluxProvider) EditImage(ctx context.Context, req ImageRequest) (*ImageResponse, error) {
	return p.runImage(ctx, req, "edit")
}

// runImage translates the OpenAI Images shape to the sidecar's and back, one
// sidecar call per requested image. The sidecar is single-flight and holds the
// model resident, so n images are sequential by construction; the gateway slot
// already serialises them anyway.
func (p *MfluxProvider) runImage(ctx context.Context, req ImageRequest, op string) (*ImageResponse, error) {
	width, height, err := parseImageSize(req.Size)
	if err != nil {
		return nil, err
	}
	if req.ResponseFormat == "url" {
		return nil, fmt.Errorf("response_format %q unsupported: the sidecar returns base64 only, use b64_json", req.ResponseFormat)
	}
	if op == "edit" && req.Mask != "" {
		return nil, fmt.Errorf("mask is not supported by the mflux sidecar")
	}

	n := req.N
	if n <= 0 {
		n = 1
	}

	out := &ImageResponse{Created: time.Now().Unix(), Data: make([]ImageDataItem, 0, n)}
	for i := 0; i < n; i++ {
		item, err := p.oneImage(ctx, req, op, width, height)
		if err != nil {
			return nil, err
		}
		out.Data = append(out.Data, item)
	}
	return out, nil
}

func (p *MfluxProvider) oneImage(ctx context.Context, req ImageRequest, op string, width, height int) (ImageDataItem, error) {
	body := map[string]interface{}{
		"prompt": req.Prompt,
		"width":  width,
		"height": height,
	}
	if op == "edit" {
		body["init_image"] = req.Image
	}
	b, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.Endpoint+"/"+op, bytes.NewBuffer(b))
	if err != nil {
		return ImageDataItem{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: imageTimeout()}
	resp, err := client.Do(httpReq)
	if err != nil {
		return ImageDataItem{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		msg := e.Error
		if msg == "" {
			msg = fmt.Sprintf("sidecar status %d", resp.StatusCode)
		}
		return ImageDataItem{}, fmt.Errorf("mflux: %s", msg)
	}

	var side struct {
		Image string `json:"image"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&side); err != nil {
		return ImageDataItem{}, err
	}
	return ImageDataItem{B64JSON: side.Image}, nil
}

// parseImageSize turns OpenAI's "WxH" size into the sidecar's width/height.
// An empty size defaults to 1024x1024 (the sidecar's own default); a size the
// sidecar could not honour is refused rather than guessed.
func parseImageSize(size string) (int, int, error) {
	if size == "" {
		return 1024, 1024, nil
	}
	var w, h int
	if _, err := fmt.Sscanf(size, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("invalid size %q: want WxH", size)
	}
	return w, h, nil
}
```

In `deploy/gateway-providers.json`, add the mflux provider and the flux-dev route:

```json
{
  "default_provider": "ollama",
  "providers": [
    { "name": "ollama", "kind": "ollama", "endpoint": "http://localhost:11434" },
    { "name": "mlx",    "kind": "mlx",    "endpoint": "http://localhost:8080" },
    { "name": "mflux",  "kind": "mflux",  "endpoint": "http://127.0.0.1:8899" }
  ],
  "models": [
    { "name": "qwen2.5-coder:3b", "provider": "mlx", "upstream": "mlx-community/Qwen2.5-Coder-3B-Instruct-4bit" },
    { "name": "LiquidAI/lfm2.5-2.6b", "provider": "ollama", "context": 65536 },
    { "name": "flux-dev", "provider": "mflux" }
  ]
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/lattice-gateway/ -run 'TestLoadProvidersRegistersMflux|TestMfluxProviderGenerates|TestAnnouncedCapabilities|TestLoadProviders' -v`
Expected: PASS, including the pre-existing `TestAnnouncedCapabilitiesChatOnlyForMLX` and `TestAnnouncedCapabilitiesAddsOllamaModalities` (the dedup rewrite is backward-compatible).

- [ ] **Step 5: Commit**

```bash
git add cmd/lattice-gateway/main.go cmd/lattice-gateway/providers.go cmd/lattice-gateway/providers_test.go deploy/gateway-providers.json
git commit -m "$(cat <<'EOF'
Add the mflux image provider and announce image_generation

Registers a new mflux provider kind that fronts the existing mflux sidecar,
exposing GenerateImage/EditImage behind an ImageProvider interface, and
announces image_generation plus the flux-dev model so the control plane can
route it with no code change.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Gateway — the `/v1/images/*` routes

**Files:**
- Modify: `cmd/lattice-gateway/main.go:227-249` (refactor `CanAccommodate` into `freeBytes` + `CanAccommodateWith`)
- Modify: `cmd/lattice-gateway/main.go` (add `imageMarginBytes`, `defaultImageModel`, `handleImage`, `handleImageGenerations`, `handleImageEdits` after `handleEmbeddings` at line 904)
- Modify: `cmd/lattice-gateway/main.go:957-964` (register the two routes in `newRouter`)
- Test: `cmd/lattice-gateway/image_test.go` (new)

**Interfaces:**
- Consumes: `ImageRequest`/`ImageResponse`/`ImageProvider`/`MfluxProvider` (Task 1), `Telemetry`, `logTelemetry`, `budgeter`, `acquireSlot`, `inferenceSlots`, `resolveModel`, `providers`, `providerMutex`, `latticeconfig.Env`.
- Produces:
  - `func (mb *MemoryBudgeter) freeBytes() uint64`
  - `func (mb *MemoryBudgeter) CanAccommodateWith(marginBytes uint64) bool`
  - `func imageMarginBytes() uint64` — default 10240 MiB, env `LATTICE_GATEWAY_IMAGE_MARGIN_MB`.
  - `const defaultImageModel = "flux-dev"`
  - `func handleImageGenerations(w, r)`, `func handleImageEdits(w, r)`, `func handleImage(w, r, op string)`

- [ ] **Step 1: Write the failing test**

Create `cmd/lattice-gateway/image_test.go`:

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

// The image route forwards the OpenAI Images body to the mflux sidecar and
// returns the sidecar's base64 as {data:[{b64_json}]}. The line is keyed from
// the header, because an image body carries no routing envelope.
func TestHandleImageGenerationsReturnsAnImage(t *testing.T) {
	var sawPath string
	var sawBody map[string]interface{}
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&sawBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image":"aW1hZ2U=","seed":1,"width":512,"height":512,"seconds":1.0}`))
	}))
	defer sidecar.Close()

	path := t.TempDir() + "/telemetry-gateway.jsonl"
	t.Setenv("LATTICE_GATEWAY_TELEMETRY", path)
	t.Setenv("LATTICE_GATEWAY_IMAGE_MARGIN_MB", "0")

	oldBudgeter, oldProviders, oldRegistry := budgeter, providers, registry
	budgeter = &MemoryBudgeter{safeMargin: 0, pageSize: 4096}
	providers = map[string]Provider{"mflux": &MfluxProvider{Endpoint: sidecar.URL}}
	registry = &providerRegistry{modelProviders: map[string]string{"flux-dev": "mflux"}}
	defer func() { budgeter, providers, registry = oldBudgeter, oldProviders, oldRegistry }()

	req := httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"flux-dev","prompt":"a cat","size":"512x512","n":1}`))
	req.Header.Set("X-Request-Id", "req-img")
	rec := httptest.NewRecorder()
	handleImageGenerations(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if sawPath != "/generate" {
		t.Errorf("forwarded to %q, want /generate", sawPath)
	}
	if sawBody["prompt"] != "a cat" {
		t.Errorf("prompt did not arrive: %v", sawBody["prompt"])
	}
	var got ImageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not an OpenAI Images object: %v", err)
	}
	if len(got.Data) != 1 || got.Data[0].B64JSON != "aW1hZ2U=" {
		t.Errorf("unexpected image response: %+v", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no telemetry line: %v", err)
	}
	var te Telemetry
	if err := json.Unmarshal(bytes.TrimSpace(raw), &te); err != nil {
		t.Fatalf("telemetry is not one JSON line: %v (%s)", err, raw)
	}
	if te.RequestID != "req-img" {
		t.Errorf("request_id = %q, want the header's value", te.RequestID)
	}
	if te.Locality != "local" {
		t.Errorf("locality = %q, want local", te.Locality)
	}
	if te.Error != "" {
		t.Errorf("error = %q, want none", te.Error)
	}
}

// The image margin is a different, larger number than the chat margin, because
// the diffusion model is ~9 GB resident. Below it the request is refused, and
// the refusal must be loud (429) and keyed.
func TestHandleImageRefusesUnderImageMemoryPressure(t *testing.T) {
	path := t.TempDir() + "/telemetry-gateway.jsonl"
	t.Setenv("LATTICE_GATEWAY_TELEMETRY", path)
	t.Setenv("LATTICE_GATEWAY_IMAGE_MARGIN_MB", "999999999")

	oldBudgeter := budgeter
	budgeter = &MemoryBudgeter{safeMargin: 0, pageSize: 4096}
	defer func() { budgeter = oldBudgeter }()

	rec := httptest.NewRecorder()
	handleImageGenerations(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"flux-dev","prompt":"a cat"}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("a refused image request wrote no telemetry: %v", err)
	}
	var te Telemetry
	if err := json.Unmarshal(bytes.TrimSpace(raw), &te); err != nil {
		t.Fatalf("telemetry is not one JSON line: %v (%s)", err, raw)
	}
	if te.Error != "memory_pressure" {
		t.Errorf("error = %q, want memory_pressure", te.Error)
	}
}

func TestRouterServesImageRoutes(t *testing.T) {
	mux := newRouter()
	for _, path := range []string{"/v1/images/generations", "/v1/images/edits"} {
		if _, pattern := mux.Handler(httptest.NewRequest("POST", path, nil)); pattern != path {
			t.Errorf("%s is served as %q — an unregistered route is a 404 to the caller", path, pattern)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/lattice-gateway/ -run 'TestHandleImageGenerations|TestHandleImageRefuses|TestRouterServesImageRoutes'`
Expected: FAIL — `undefined: handleImageGenerations`, `undefined: CanAccommodateWith` (the image-margin knob does not exist yet).

- [ ] **Step 3: Write the minimal implementation**

Refactor `CanAccommodate` (line 227-249) — extract the `vm_stat` read into `freeBytes`, keep `CanAccommodate` as-is for callers, add `CanAccommodateWith`. **Preserve the existing `vm_stat` parse verbatim** (the code below is the target shape; if the current parse differs in detail — e.g. it strips a trailing `.` before parsing — keep the existing lines, since `freeBytes` must return the exact same number `CanAccommodate` computes today):

```go
// freeBytes reads the host's free + inactive memory from vm_stat, converted by
// page size. Extracted from CanAccommodate so a caller can ask the same question
// against a different margin: the image model is far larger than a chat model,
// so its margin is a different number, not a different check.
func (mb *MemoryBudgeter) freeBytes() uint64 {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0
	}
	lines := strings.Split(string(out), "\n")
	var freePages, inactivePages uint64
	for _, line := range lines {
		if strings.Contains(line, "Pages free") {
			fmt.Sscanf(line, "Pages free: %d", &freePages)
		} else if strings.Contains(line, "Pages inactive") {
			fmt.Sscanf(line, "Pages inactive: %d", &inactivePages)
		}
	}
	return (freePages + inactivePages) * mb.pageSize
}

func (mb *MemoryBudgeter) CanAccommodate() bool {
	return mb.freeBytes() > mb.safeMargin
}

// CanAccommodateWith is CanAccommodate against a caller-supplied margin. The
// image handler uses it with the image margin, which is an order of magnitude
// larger than the chat margin because the diffusion model is ~9 GB resident.
func (mb *MemoryBudgeter) CanAccommodateWith(marginBytes uint64) bool {
	return mb.freeBytes() > marginBytes
}
```

Add after `handleEmbeddings` (line 904), before `handleHealth`:

```go
// defaultImageModel is the Lattice alias for the diffusion model. An image
// request that omits model gets this rather than failing on an empty name.
const defaultImageModel = "flux-dev"

// imageMarginBytes is the free-RAM floor below which an image request is
// refused. It is separate from the chat margin (LATTICE_GATEWAY_MEMORY_MARGIN_MB)
// because the diffusion model is ~9 GB resident — far larger than any chat
// model — so a chat-sized margin would admit an image load that thrashes swap.
func imageMarginBytes() uint64 {
	mb := 10240
	if v := latticeconfig.Env("LATTICE_GATEWAY_IMAGE_MARGIN_MB", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			mb = n
		}
	}
	return uint64(mb) * 1024 * 1024
}

func handleImageGenerations(w http.ResponseWriter, r *http.Request) {
	handleImage(w, r, "generate")
}

func handleImageEdits(w http.ResponseWriter, r *http.Request) {
	handleImage(w, r, "edit")
}

// handleImage mirrors handleEmbeddings: the same single slot, the same
// telemetry-on-every-exit discipline, but against the image memory margin and
// the mflux provider. The id rides the X-Request-Id header, exactly as an
// embedding's does — an image body carries no routing envelope.
func handleImage(w http.ResponseWriter, r *http.Request, op string) {
	t0 := time.Now()
	requestID := r.Header.Get("X-Request-Id")

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()})
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var img ImageRequest
	if err := json.Unmarshal(bodyBytes, &img); err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()})
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if img.Model == "" {
		img.Model = defaultImageModel
	}

	if !budgeter.CanAccommodateWith(imageMarginBytes()) {
		fmt.Println("  Memory pressure: available RAM below image margin. Rejecting to avoid swap.")
		logTelemetry(Telemetry{
			RequestID: requestID,
			Model:     img.Model,
			Elapsed:   time.Since(t0).Seconds(),
			Error:     "memory_pressure",
		})
		http.Error(w, "Local memory pressure: available RAM below image margin", http.StatusTooManyRequests)
		return
	}

	if !acquireSlot(r.Context()) {
		logTelemetry(Telemetry{
			RequestID: requestID,
			Model:     img.Model,
			Elapsed:   time.Since(t0).Seconds(),
			Error:     "client_cancelled_while_queued",
		})
		return
	}
	defer func() { <-inferenceSlots }()

	providerName, _ := resolveModel(img.Model)
	providerMutex.RLock()
	provider := providers[providerName]
	providerMutex.RUnlock()
	if provider == nil {
		logTelemetry(Telemetry{RequestID: requestID, Model: img.Model, Elapsed: time.Since(t0).Seconds(), Error: "no provider registered for model"})
		http.Error(w, "No provider registered for model", http.StatusInternalServerError)
		return
	}

	ip, ok := provider.(ImageProvider)
	if !ok {
		logTelemetry(Telemetry{RequestID: requestID, Model: img.Model, Elapsed: time.Since(t0).Seconds(), Error: fmt.Sprintf("provider %q is not an image provider", providerName)})
		http.Error(w, "Provider is not an image provider", http.StatusInternalServerError)
		return
	}

	var res *ImageResponse
	if op == "edit" {
		res, err = ip.EditImage(r.Context(), img)
	} else {
		res, err = ip.GenerateImage(r.Context(), img)
	}

	te := Telemetry{RequestID: requestID, Model: img.Model, Elapsed: time.Since(t0).Seconds()}
	if err != nil {
		te.Error = err.Error()
		logTelemetry(te)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	logTelemetry(te)

	// Set this explicitly: without it Go sniffs the JSON body as text/plain,
	// which strict OpenAI clients reject.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}
```

Register the routes in `newRouter` (after the embeddings line, 960):

```go
	mux.HandleFunc("/v1/images/generations", handleImageGenerations)
	mux.HandleFunc("/v1/images/edits", handleImageEdits)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/lattice-gateway/ -v`
Expected: PASS — the three new tests plus every pre-existing gateway test (the `freeBytes` refactor preserves `CanAccommodate`'s behaviour, so the embeddings memory tests still pass).

- [ ] **Step 5: Commit**

```bash
git add cmd/lattice-gateway/main.go cmd/lattice-gateway/image_test.go
git commit -m "$(cat <<'EOF'
Serve the OpenAI Images routes on the gateway

Mirror handleEmbeddings for image generation: a dedicated image memory margin,
the shared single slot, and a deferred telemetry line keyed by X-Request-Id,
proxying to the mflux provider behind a new /v1/images/generations and
/v1/images/edits pair.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Frontend — the `/v1/images/*` routes

**Files:**
- Modify: `cmd/lattice-frontend/main.go:181` (add image handlers after `handleEmbeddings`)
- Modify: `cmd/lattice-frontend/main.go:294-301` (register the two routes in `newRouter`)
- Test: `cmd/lattice-frontend/handler_test.go`

**Interfaces:**
- Consumes: `proxyInference(w, r, upstreamPath string, injectRouting bool)`, `Decision`, `controlURL`, `telemetryPath`, `newRouter`.
- Produces: `func handleImageGenerations(w, r)`, `func handleImageEdits(w, r)`.

> **Note on `proxyInference`:** it parses `model` from the body for the control call, then `buildProxyBody` re-marshals the *original* body as a map, forwarding `prompt`/`n`/`size` untouched and injecting no routing envelope (`injectRouting=false`). No new frontend logic is needed — the image handlers are the embeddings handlers with a different upstream path.

- [ ] **Step 1: Write the failing test**

Add to `cmd/lattice-frontend/handler_test.go` (modelled on `TestHandleEmbeddingsForwardsWithoutARoutingEnvelope`, line 390):

```go
// An image is not a chat. The upstream path must be /v1/images/generations, the
// client's own body must arrive intact, and no routing envelope may be injected.
// The id rides a header, so the gateway's own line stays keyed.
func TestHandleImageForwardsWithoutARoutingEnvelope(t *testing.T) {
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
			ModelName: "flux-dev",
		})
	}))
	defer control.Close()

	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawHeader = r.Header.Get("X-Request-Id")
		json.NewDecoder(r.Body).Decode(&sawBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"created":1,"data":[{"b64_json":"aW1hZ2U="}]}`))
	}))
	defer target.Close()

	oldControl, oldTelemetry := controlURL, telemetryPath
	controlURL = control.URL
	telemetryPath = t.TempDir() + "/telemetry-frontend.jsonl"
	defer func() { controlURL, telemetryPath = oldControl, oldTelemetry }()

	body := `{"model":"flux-dev","prompt":"a cat","size":"512x512","n":1}`
	rec := httptest.NewRecorder()
	handleImageGenerations(rec, httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if sawPath != "/v1/images/generations" {
		t.Errorf("upstream path = %q, want /v1/images/generations", sawPath)
	}
	if sawBody["prompt"] != "a cat" {
		t.Errorf("prompt did not arrive intact: %v", sawBody["prompt"])
	}
	if sawBody["size"] != "512x512" {
		t.Errorf("size was dropped: %v", sawBody)
	}
	if _, ok := sawBody["routing"]; ok {
		t.Errorf("a routing envelope reached the image path: %v", sawBody["routing"])
	}

	logged, err := os.ReadFile(telemetryPath)
	if err != nil {
		t.Fatalf("no frontend telemetry line: %v", err)
	}
	var line Telemetry
	if err := json.Unmarshal(bytes.TrimSpace(logged), &line); err != nil {
		t.Fatalf("frontend telemetry is not one JSON line: %v (%s)", err, logged)
	}
	if sawHeader != line.RequestID {
		t.Errorf("X-Request-Id = %q, want %q — the gateway's line would be unkeyed",
			sawHeader, line.RequestID)
	}
	if line.Locality != "local" {
		t.Errorf("frontend locality = %q, want local", line.Locality)
	}
}
```

Add the image paths to `TestRouterServesEveryClientFacingRoute` (line 466-473), changing the slice to:

```go
	for _, path := range []string{"/v1/chat/completions", "/v1/embeddings", "/v1/images/generations", "/v1/images/edits", "/v1/models", "/health"} {
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/lattice-frontend/ -run 'TestHandleImageForwards|TestRouterServesEveryClientFacingRoute'`
Expected: FAIL — `undefined: handleImageGenerations`, and the route assertion reports the image paths as served by `""`.

- [ ] **Step 3: Write the minimal implementation**

In `cmd/lattice-frontend/main.go`, after `handleEmbeddings` (line 181):

```go
// handleImageGenerations shares the chat flow — parse, ask control, proxy — and
// differs in exactly two ways: the upstream path, and the absence of a routing
// envelope (an image body is not the chat translation layer's contract). The
// image model's untagged name routes local through control's existing rule.
func handleImageGenerations(w http.ResponseWriter, r *http.Request) {
	proxyInference(w, r, "/v1/images/generations", false)
}

func handleImageEdits(w http.ResponseWriter, r *http.Request) {
	proxyInference(w, r, "/v1/images/edits", false)
}
```

Register in `newRouter` (after the embeddings line, 297):

```go
	mux.HandleFunc("/v1/images/generations", handleImageGenerations)
	mux.HandleFunc("/v1/images/edits", handleImageEdits)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/lattice-frontend/ -v`
Expected: PASS — the new test plus every pre-existing frontend test.

- [ ] **Step 5: Commit**

```bash
git add cmd/lattice-frontend/main.go cmd/lattice-frontend/handler_test.go
git commit -m "$(cat <<'EOF'
Route the OpenAI Images routes through the frontend

Add /v1/images/generations and /v1/images/edits as thin proxyInference
variants: the image body is forwarded intact with no routing envelope, and
control still decides the target for the untagged image model.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: The MCP server

**Files:**
- Create: `deploy/dsh/mcp-imagegen/server.py`
- Create: `deploy/dsh/mcp-imagegen/test_server.py`
- Create: `deploy/dsh/mcp-imagegen/README.md`

**Interfaces:**
- Consumes: the frontend's `POST /v1/images/generations` + `/v1/images/edits` (Tasks 2-3), the `mcp` Python SDK, `LATTICE_FRONTEND_URL`, `LATTICE_IMAGE_TIMEOUT_S`.
- Produces:
  - MCP server `lattice-imagegen` with tools `generate_image(prompt, size?, n?)` and `generate_image_edit(image_b64, prompt, size?, n?)`, each returning a `list[ImageContent]`.

> **Dependency:** `pip install mcp` (the official SDK). The server uses `FastMCP` (the stable import across SDK 1.x and 2.x) and `ImageContent` from `mcp.types`. The MIME-type field on `ImageContent` is `mime_type` in the current SDK and `mimeType` in the legacy FastMCP line — match the installed version; the wire value is `mimeType` either way.

- [ ] **Step 1: Write the server**

Create `deploy/dsh/mcp-imagegen/server.py`:

```python
#!/usr/bin/env python3
"""Lattice image-generation MCP server — fronts Lattice's /v1/images routes.

Run as a stdio child of DSH's dsh-mcp-client (transport: stdio). It exposes two
model-callable tools, generate_image and generate_image_edit, each of which POSTs
to the Lattice frontend's OpenAI Images route and returns the image as an MCP
image content block. It computes nothing itself: Lattice's gateway owns the
single slot, the memory margin and the routing; this process is only the
protocol adapter that turns an MCP tool call into an HTTP POST.

Requires the `mcp` package:  pip install mcp
Run:                          python server.py   (stdio)
"""

from __future__ import annotations

import json
import os
import urllib.request

from mcp.server.fastmcp import FastMCP
from mcp.types import ImageContent

FRONTEND = os.environ.get("LATTICE_FRONTEND_URL", "http://127.0.0.1:8080")
TIMEOUT = int(os.environ.get("LATTICE_IMAGE_TIMEOUT_S", "7200"))

mcp = FastMCP("lattice-imagegen")


def _image(b64: str) -> ImageContent:
    return ImageContent(type="image", data=b64, mime_type="image/png")


def _generate(prompt: str, size: str | None, n: int, model: str = "flux-dev") -> list[ImageContent]:
    body = {"model": model, "prompt": prompt, "n": n, "response_format": "b64_json"}
    if size:
        body["size"] = size
    req = urllib.request.Request(
        f"{FRONTEND}/v1/images/generations",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
        payload = json.load(resp)
    return [_image(item["b64_json"]) for item in payload["data"]]


def _edit(image_b64: str, prompt: str, size: str | None, n: int, model: str = "flux-dev") -> list[ImageContent]:
    body = {"model": model, "image": image_b64, "prompt": prompt, "n": n, "response_format": "b64_json"}
    if size:
        body["size"] = size
    req = urllib.request.Request(
        f"{FRONTEND}/v1/images/edits",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
        payload = json.load(resp)
    return [_image(item["b64_json"]) for item in payload["data"]]


@mcp.tool()
def generate_image(prompt: str, size: str = "512x512", n: int = 1) -> list[ImageContent]:
    """Generate an image from a text prompt via Lattice's local FLUX model.

    size is WxH in pixels ("256x256", "512x512", "1024x1024"); smaller is much
    faster. n is the number of images. Returns the image(s) as PNG content.
    """
    return _generate(prompt, size, n)


@mcp.tool()
def generate_image_edit(image_b64: str, prompt: str, size: str = "512x512", n: int = 1) -> list[ImageContent]:
    """Edit an existing image (base64) guided by a text prompt, via Lattice's FLUX."""
    return _edit(image_b64, prompt, size, n)


if __name__ == "__main__":
    mcp.run()
```

- [ ] **Step 2: Write the smoke test**

Create `deploy/dsh/mcp-imagegen/test_server.py`:

```python
#!/usr/bin/env python3
"""Smoke test for the MCP server's HTTP adapter — no MCP client needed.

Spins up a stdlib mock of the Lattice frontend and asserts that generate_image's
helper POSTs the OpenAI Images body and turns the response into image content
blocks. Run:  python3 test_server.py
"""

import http.server
import json
import os
import threading

_mock = {}


class _Frontend(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        _mock["path"] = self.path
        _mock["body"] = json.loads(self.rfile.read(length) or b"{}")
        body = json.dumps({"created": 1, "data": [{"b64_json": "aGVsbG8="}]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


def main():
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _Frontend)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    os.environ["LATTICE_FRONTEND_URL"] = f"http://127.0.0.1:{srv.server_port}"
    os.environ["LATTICE_IMAGE_TIMEOUT_S"] = "5"

    import server  # reads the env above at import time

    blocks = server._generate("a cat", "512x512", 1)
    assert _mock["path"] == "/v1/images/generations", _mock["path"]
    assert _mock["body"] == {"model": "flux-dev", "prompt": "a cat", "n": 1,
                             "response_format": "b64_json", "size": "512x512"}, _mock["body"]
    assert len(blocks) == 1
    assert blocks[0].type == "image"
    assert blocks[0].data == "aGVsbG8="
    print("ok")


if __name__ == "__main__":
    main()
```

- [ ] **Step 3: Run the smoke test**

Run: `python3 deploy/dsh/mcp-imagegen/test_server.py`
Expected: PASS — prints `ok`. (Requires `pip install mcp` first; if the installed SDK's `ImageContent` field is `mimeType`, adjust `_image` to use that kwarg.)

- [ ] **Step 4: Write the README**

Create `deploy/dsh/mcp-imagegen/README.md`:

```markdown
# lattice-imagegen — MCP server for Lattice image generation

A stdio [MCP](https://modelcontextprotocol.io) server that fronts Lattice's
OpenAI Images route so DSH (or any MCP client) can generate and edit images as
model-callable tools. It computes nothing: the Lattice gateway owns the slot,
the memory margin and the routing.

## Install

```bash
python3 -m venv .venv
.venv/bin/pip install mcp
```

## Run (stdio)

```bash
LATTICE_FRONTEND_URL=http://127.0.0.1:8080 .venv/bin/python server.py
```

The server reads stdin/stdout; DSH spawns it as a subprocess.

## Tools

- `generate_image(prompt, size="512x512", n=1)` → PNG image content.
- `generate_image_edit(image_b64, prompt, size="512x512", n=1)` → PNG image content.

## Configuration

- `LATTICE_FRONTEND_URL` — the Lattice frontend (default `http://127.0.0.1:8080`).
- `LATTICE_IMAGE_TIMEOUT_S` — HTTP client timeout (default `7200`); must exceed a
  1024² generation (~1 h).

## Test

```bash
python3 test_server.py   # prints "ok"
```
```

- [ ] **Step 5: Commit**

```bash
git add deploy/dsh/mcp-imagegen/
git commit -m "$(cat <<'EOF'
Add the lattice-imagegen MCP server

A thin stdio MCP server that fronts Lattice's /v1/images routes with two
model-callable tools, returning the sidecar's base64 as MCP image content
blocks. It is a client adapter, not Lattice infrastructure.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: DSH wiring, docs, and end-to-end acceptance

**Files:**
- Create: `deploy/dsh/mcp-imagegen/cordis.patch.yml.example`
- Modify: `docs/specs/lattice-image-gateway.md:233-248` (mark decision log entries implemented, not Draft)

**Interfaces:**
- Consumes: the MCP server (Task 4), the deployed gateway/frontend/control (Tasks 1-3), the DSH `dsh-mcp-client` plugin.
- Produces: a copyable DSH wiring sample; the spec's status bump.

> **Note on the DSH config schema:** the `dsh-mcp-client` plugin's `config` is a zod union — stdio carries `serverName`/`transport`/`command`/`args`/`env`, streamable-http carries `url`/`headers`. `toolCallTimeoutMs` raises the plugin's 60,000 ms default; confirm its exact key location against the installed `dsh-mcp-client` README when wiring. This is a Pi-side file, not committed — only the `.example` is tracked.

- [ ] **Step 1: Write the wiring sample**

Create `deploy/dsh/mcp-imagegen/cordis.patch.yml.example`:

```yaml
# Example DSH cordis.patch.yml — copy to ~/.dsh/profiles/<profile>/cordis.patch.yml
# and fill the absolute path to server.py. Declares the lattice-imagegen MCP
# server as a model-callable tool provider over stdio, with a timeout that
# outlasts a 1024² generation (the 60 s default kills every image before it
# finishes).
- id: lattice-imagegen
  name: '@deepseek-ai/dsh-mcp-client'
  config:
    serverName: lattice-imagegen
    transport: stdio
    command: python3
    args:
      - /absolute/path/to/deploy/dsh/mcp-imagegen/server.py
    toolCallTimeoutMs: 3600000
```

- [ ] **Step 2: Bump the spec status**

In `docs/specs/lattice-image-gateway.md`, change the `Status:` line (line 3) from `Draft` to `Implemented`, and append a decision-log entry (after line 248):

```markdown
- **2026-10-01 — implemented.** Tasks 1-5 of
  [the plan](../plans/2026-10-01-image-gateway-mcp.md): gateway image routes,
  the mflux provider, frontend routes, and the MCP server. The decision log's
  three entries above stand unchanged.
```

- [ ] **Step 3: Verify the spec's acceptance criteria**

Run, in order (the first four are the spec §12 acceptance):

1. `go test ./...` and `go vet ./...` — all packages pass (gateway + frontend + control unchanged but rebuilt).
2. `go build ./...` — binaries build with Go stdlib only.
3. On the Mac, with the mflux sidecar running: `curl -s -X POST http://127.0.0.1:8080/v1/images/generations -H 'Content-Type: application/json' -d '{"model":"flux-dev","prompt":"a cat","size":"512x512","n":1}'` → `{"created":…,"data":[{"b64_json":"…"}]}`; a concurrent chat request queues behind the image slot (no concurrent resident models).
4. Confirm control telemetry records `target: mac-gateway`, `model: flux-dev`, `capability: local`; confirm an irreconcilable image request fails loudly by name (e.g. a cloud-tagged model).
5. On the Pi, wire `cordis.patch.yml`, confirm DSH lists `mcp__lattice-imagegen__generate_image`, and run a live DSH turn that returns an image into native context without a 60 s timeout.

- [ ] **Step 4: Commit**

```bash
git add deploy/dsh/mcp-imagegen/cordis.patch.yml.example docs/specs/lattice-image-gateway.md
git commit -m "$(cat <<'EOF'
Document the DSH MCP wiring and mark the image gateway implemented

Add the cordis.patch.yml example for dsh-mcp-client and bump the spec status,
completing the image-gateway + MCP-server scope.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

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
	// 1 MiB, not 0: imageMarginBytes rejects a non-positive value and falls back
	// to the 10 GiB default, which would make this test depend on the machine's
	// real free memory. 1 MiB is below any host's free memory, so the check passes.
	t.Setenv("LATTICE_GATEWAY_IMAGE_MARGIN_MB", "1")

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

func TestHandleImageEditsForwardsTheSourceImage(t *testing.T) {
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
	t.Setenv("LATTICE_GATEWAY_IMAGE_MARGIN_MB", "1")

	oldBudgeter, oldProviders, oldRegistry := budgeter, providers, registry
	budgeter = &MemoryBudgeter{safeMargin: 0, pageSize: 4096}
	providers = map[string]Provider{"mflux": &MfluxProvider{Endpoint: sidecar.URL}}
	registry = &providerRegistry{modelProviders: map[string]string{"flux-dev": "mflux"}}
	defer func() { budgeter, providers, registry = oldBudgeter, oldProviders, oldRegistry }()

	req := httptest.NewRequest("POST", "/v1/images/edits",
		strings.NewReader(`{"model":"flux-dev","image":"aW1hZ2U=","prompt":"a cat","size":"512x512","n":1}`))
	req.Header.Set("X-Request-Id", "req-img-edit")
	rec := httptest.NewRecorder()
	handleImageEdits(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if sawPath != "/edit" {
		t.Errorf("forwarded to %q, want /edit", sawPath)
	}
	if sawBody["init_image"] != "aW1hZ2U=" {
		t.Errorf("init_image did not arrive: %v", sawBody["init_image"])
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

// An image model with an upstream mapping names a host-local bake the sidecar
// must load; the mapping must ride the request as the sidecar's model field.
func TestHandleImageRoutesTheSidecarModelFromUpstream(t *testing.T) {
	var sawBody map[string]interface{}
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&sawBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image":"aW1hZ2U=","seed":1,"width":512,"height":512,"seconds":1.0}`))
	}))
	defer sidecar.Close()

	path := t.TempDir() + "/telemetry-gateway.jsonl"
	t.Setenv("LATTICE_GATEWAY_TELEMETRY", path)
	t.Setenv("LATTICE_GATEWAY_IMAGE_MARGIN_MB", "1")

	oldBudgeter, oldProviders, oldRegistry := budgeter, providers, registry
	budgeter = &MemoryBudgeter{safeMargin: 0, pageSize: 4096}
	providers = map[string]Provider{"mflux": &MfluxProvider{Endpoint: sidecar.URL}}
	registry = &providerRegistry{
		modelProviders: map[string]string{"flux-uncensored": "mflux"},
		modelUpstream:  map[string]string{"flux-uncensored": "/models/persephone-4bit"},
	}
	defer func() { budgeter, providers, registry = oldBudgeter, oldProviders, oldRegistry }()

	req := httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"flux-uncensored","prompt":"a cat","size":"512x512","n":1}`))
	req.Header.Set("X-Request-Id", "req-img-upstream")
	rec := httptest.NewRecorder()
	handleImageGenerations(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if sawBody["model"] != "/models/persephone-4bit" {
		t.Errorf("upstream did not arrive as sidecar model: %v", sawBody["model"])
	}
}

// A model without an upstream mapping keeps the sidecar on its env-default
// model: forwarding the registry name would send mflux a path it cannot
// resolve. The client-sent model field is likewise never trusted.
func TestHandleImageDoesNotForwardAModelWithoutAnUpstream(t *testing.T) {
	var sawBody map[string]interface{}
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&sawBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image":"aW1hZ2U=","seed":1,"width":512,"height":512,"seconds":1.0}`))
	}))
	defer sidecar.Close()

	path := t.TempDir() + "/telemetry-gateway.jsonl"
	t.Setenv("LATTICE_GATEWAY_TELEMETRY", path)
	t.Setenv("LATTICE_GATEWAY_IMAGE_MARGIN_MB", "1")

	oldBudgeter, oldProviders, oldRegistry := budgeter, providers, registry
	budgeter = &MemoryBudgeter{safeMargin: 0, pageSize: 4096}
	providers = map[string]Provider{"mflux": &MfluxProvider{Endpoint: sidecar.URL}}
	registry = &providerRegistry{modelProviders: map[string]string{"flux-dev": "mflux"}}
	defer func() { budgeter, providers, registry = oldBudgeter, oldProviders, oldRegistry }()

	req := httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"flux-dev","prompt":"a cat","size":"512x512","n":1}`))
	req.Header.Set("X-Request-Id", "req-img-noupstream")
	rec := httptest.NewRecorder()
	handleImageGenerations(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if _, ok := sawBody["model"]; ok {
		t.Errorf("model forwarded without an upstream mapping: %v", sawBody["model"])
	}
	if _, ok := sawBody["loras"]; ok {
		t.Errorf("loras forwarded when the client sent none: %v", sawBody["loras"])
	}
}

// Lora refs are a Lattice extension on the images body; they must reach the
// sidecar in its [{name, scale}] shape.
func TestHandleImageForwardsLoras(t *testing.T) {
	var sawBody map[string]interface{}
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&sawBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image":"aW1hZ2U=","seed":1,"width":512,"height":512,"seconds":1.0}`))
	}))
	defer sidecar.Close()

	path := t.TempDir() + "/telemetry-gateway.jsonl"
	t.Setenv("LATTICE_GATEWAY_TELEMETRY", path)
	t.Setenv("LATTICE_GATEWAY_IMAGE_MARGIN_MB", "1")

	oldBudgeter, oldProviders, oldRegistry := budgeter, providers, registry
	budgeter = &MemoryBudgeter{safeMargin: 0, pageSize: 4096}
	providers = map[string]Provider{"mflux": &MfluxProvider{Endpoint: sidecar.URL}}
	registry = &providerRegistry{modelProviders: map[string]string{"flux-dev": "mflux"}}
	defer func() { budgeter, providers, registry = oldBudgeter, oldProviders, oldRegistry }()

	req := httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"flux-dev","prompt":"a cat","size":"512x512","n":1,"loras":[{"name":"lustly","scale":0.8}]}`))
	req.Header.Set("X-Request-Id", "req-img-loras")
	rec := httptest.NewRecorder()
	handleImageGenerations(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var loras []ImageLora
	b, _ := json.Marshal(sawBody["loras"])
	if err := json.Unmarshal(b, &loras); err != nil || len(loras) != 1 {
		t.Fatalf("loras did not arrive: %s (%v)", b, err)
	}
	if loras[0].Name != "lustly" || loras[0].Scale != 0.8 {
		t.Errorf("loras = %+v, want [{lustly 0.8}]", loras)
	}
}

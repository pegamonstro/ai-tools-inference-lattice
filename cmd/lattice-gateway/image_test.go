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

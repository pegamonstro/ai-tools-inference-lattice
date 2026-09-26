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

func TestRouterServesEveryRoute(t *testing.T) {
	mux := newRouter()
	for _, path := range []string{"/v1/chat/completions", "/v1/embeddings", "/health", "/telemetry"} {
		if _, pattern := mux.Handler(httptest.NewRequest("POST", path, nil)); pattern != path {
			t.Errorf("%s is served as %q — an unregistered route is a 404 to the caller", path, pattern)
		}
	}
}

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// underPressureBudgeter guarantees CanAccommodate() is false without depending
// on the host's vm_stat reading: the safety margin exceeds any machine's RAM,
// so the measured reclaimable pool always lands below it.
func underPressureBudgeter() *MemoryBudgeter {
	return &MemoryBudgeter{safeMargin: 1 << 45, pageSize: 16384}
}

// fakeOllamaTags stands in for the local ollama so the health/models handlers
// reach their tag-discovery step in tests.
func fakeOllamaTags(t *testing.T) *httptest.Server {
	t.Helper()
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[{"name":"gemma4:12b","size":8589934592}]}`))
	}))
	t.Cleanup(ollama.Close)
	return ollama
}

// The control plane polls /health and drops an unhealthy gateway from every
// capability for a whole poll cycle. A render filling RAM used to 503 the
// memory gate inside this handler and cascade to "no adequate gateway" for
// unrelated jobs. Liveness here means ollama; memory pressure degrades through
// admission and the slot ceiling, which see the same budgeter at request time.
func TestHandleHealthIsLivenessOnly(t *testing.T) {
	ollama := fakeOllamaTags(t)
	oldOllama, oldBudgeter := ollamaURL, budgeter
	ollamaURL = ollama.URL
	budgeter = underPressureBudgeter()
	defer func() { ollamaURL, budgeter = oldOllama, oldBudgeter }()

	rec := httptest.NewRecorder()
	handleHealth(rec, httptest.NewRequest("GET", "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — health must not flap under memory pressure (%s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		Status string `json:"status"`
		Slots  int    `json:"slots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("health is not JSON: %v (%s)", err, rec.Body.String())
	}
	if payload.Status != "ok" {
		t.Errorf("status = %q, want ok", payload.Status)
	}
	if payload.Slots < 1 {
		t.Errorf("slots = %d, want >= 1 — the computeSlots floor keeps the capacity signal usable", payload.Slots)
	}
}

// A model menu is informational: refusing it on memory pressure only breaks
// clients that negotiate menus between jobs, the way img-gen's model picker did.
func TestHandleModelsServesUnderMemoryPressure(t *testing.T) {
	ollama := fakeOllamaTags(t)
	oldOllama, oldBudgeter := ollamaURL, budgeter
	ollamaURL = ollama.URL
	budgeter = underPressureBudgeter()
	defer func() { ollamaURL, budgeter = oldOllama, oldBudgeter }()

	rec := httptest.NewRecorder()
	handleModels(rec, httptest.NewRequest("GET", "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a menu response must not depend on memory pressure (%s)", rec.Code, rec.Body.String())
	}
}

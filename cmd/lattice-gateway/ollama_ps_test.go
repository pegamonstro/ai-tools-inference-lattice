package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// pollOllamaPS turns ollama /api/ps into the residency snapshot the wired
// ledger consults: the sum of every resident model's size_vram. The tests
// re-point the ollamaURL global the same way the /api/tags tests do.
func TestPollOllamaPSSumsResidentVRAM(t *testing.T) {
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			t.Errorf("polled %q, want /api/ps", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[
			{"name":"dolphin3","size_vram":5000000000},
			{"name":"qwen2","size_vram":3000000000}
		]}`))
	}))
	defer ps.Close()

	old := ollamaURL
	ollamaURL = ps.URL
	defer func() { ollamaURL = old }()

	pollOllamaPS()
	if got := ollamaPSResidentBytes(); got != 8_000_000_000 {
		t.Fatalf("resident = %d, want 8000000000 (sum of both models)", got)
	}
}

// A model without VRAM residency (a load in progress reports 0) must not
// contribute a fake number to the budget.
func TestPollOllamaPSSkipsUnsizedModels(t *testing.T) {
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[
			{"name":"dolphin3","size_vram":5000000000},
			{"name":"loading","size_vram":0},
			{"name":"weird","size_vram":-5}
		]}`))
	}))
	defer ps.Close()

	old := ollamaURL
	ollamaURL = ps.URL
	defer func() { ollamaURL = old }()

	pollOllamaPS()
	if got := ollamaPSResidentBytes(); got != 5_000_000_000 {
		t.Fatalf("resident = %d, want 5000000000 (only sized models count)", got)
	}
}

// Poll failures retain the last snapshot — the ledger may over-count briefly
// after an outage but never under-counts, which would re-open the gate while
// ollama's state is unknown.
func TestPollOllamaPSFailureKeepsLast(t *testing.T) {
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[{"name":"dolphin3","size_vram":5000000000}]}`))
	}))
	old := ollamaURL
	defer func() { ollamaURL = old }()

	ollamaURL = ps.URL
	pollOllamaPS()
	if got := ollamaPSResidentBytes(); got != 5_000_000_000 {
		t.Fatalf("precondition: resident = %d", got)
	}

	ps.Close() // dead target: connection refused
	pollOllamaPS()
	if got := ollamaPSResidentBytes(); got != 5_000_000_000 {
		t.Fatalf("failed poll clobbered the snapshot: resident = %d, want 5000000000", got)
	}

	// A malformed body is the same failure mode: keep the last good value.
	ps2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models": `))
	}))
	defer ps2.Close()
	ollamaURL = ps2.URL
	pollOllamaPS()
	if got := ollamaPSResidentBytes(); got != 5_000_000_000 {
		t.Fatalf("malformed body clobbered the snapshot: resident = %d", got)
	}
}

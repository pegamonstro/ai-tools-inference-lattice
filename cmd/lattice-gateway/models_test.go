package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouterServesTheModelsRoute(t *testing.T) {
	mux := newRouter()
	if _, pattern := mux.Handler(httptest.NewRequest("GET", "/v1/models", nil)); pattern != "/v1/models" {
		t.Errorf("/v1/models is served as %q — an unregistered route is a 404 to the caller", pattern)
	}
}

type modelEntry struct {
	ID         string `json:"id"`
	Object     string `json:"object"`
	OwnedBy    string `json:"owned_by"`
	ImageModel bool   `json:"image_model"`
}

// The discovery contract a probing client lives by: one /v1/models that merges
// the whole served zoo — chat, embeddings, image — with image models marked, so
// a model list can be read as a menu, not as a guess.
func TestHandleModelsMergesChatAndImageModels(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[{"name":"gemma4:12b","size":8000000000},{"name":"gemma4:31b","size":0},{"name":"gemma4:31b-cloud","size":0}]}`))
	}))
	defer ollama.Close()

	oldOllama, oldBudgeter, oldRegistry := ollamaURL, budgeter, registry
	ollamaURL = ollama.URL
	budgeter = &MemoryBudgeter{safeMargin: 0, pageSize: 4096}
	registry = &providerRegistry{
		defaultProvider: "ollama",
		served: map[string][]string{
			"ollama": {"gemma4:12b", "qwen3.8:27b"},
			"mflux":  {"flux-uncensored"},
		},
		providerKinds: map[string]string{"ollama": "ollama", "mflux": "mflux"},
	}
	defer func() { ollamaURL, budgeter, registry = oldOllama, oldBudgeter, oldRegistry }()

	rec := httptest.NewRecorder()
	handleModels(rec, httptest.NewRequest("GET", "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q — Go sniffs JSON as text/plain without an explicit header", ct)
	}

	var got struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not OpenAI list JSON: %v (%s)", err, rec.Body.String())
	}
	if got.Object != "list" {
		t.Errorf("object = %q, want list", got.Object)
	}

	byID := map[string]modelEntry{}
	for _, e := range got.Data {
		if _, dup := byID[e.ID]; dup {
			t.Errorf("%s appears twice — the merged list must dedupe", e.ID)
		}
		byID[e.ID] = e
	}

	var entry modelEntry
	found := func(id string) bool {
		var ok bool
		if entry, ok = byID[id]; !ok {
			t.Errorf("%s is absent from /v1/models: %+v", id, got.Data)
		}
		return ok
	}
	if found("gemma4:31b") && (entry.Object != "model" || entry.OwnedBy != "lattice") {
		t.Errorf("gemma4:31b entry = %s/%s, want model/lattice", entry.OwnedBy, entry.Object)
	}
	for id, wantImage := range map[string]bool{
		"gemma4:12b":      false,
		"qwen3.8:27b":     false,
		"flux-uncensored": true,
	} {
		if !found(id) {
			continue
		}
		if entry.ImageModel != wantImage {
			t.Errorf("%s image_model = %v, want %v", id, entry.ImageModel, wantImage)
		}
	}
	if _, ok := byID["gemma4:31b-cloud"]; ok {
		t.Error("cloud tag reached /v1/models — cloud aliases are not servable models")
	}
}

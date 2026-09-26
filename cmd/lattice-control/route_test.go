package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

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

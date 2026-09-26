package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
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

// Ollama marks a cloud-hosted model in the tag: `<size>-cloud`, or `cloud` when
// there is no size. Reading that marker is how a client that can send no
// routing envelope still gets routed to the cloud instead of failing at the Mac.
func TestIsCloudModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"deepseek-v4.1-flash:cloud", true},
		{"nemotron-3-nano:30b-cloud", true},
		{"glm-5.3-flash:cloud", true},
		{"namespace/gpt-oss:120b-cloud", true},
		{"granite4:3b", false},
		{"hermes3:8b", false},
		{"llama3.2:3b", false},
		// A capability alias is policy's to resolve, not a literal model name.
		{"local-brain", false},
		// A tag must be present to carry the marker. Guessing from an untagged
		// name risks sending a local model to the cloud, which the privacy rule
		// forbids — so an untagged name stays local and fails loudly instead.
		{"mystery-cloud", false},
		{"foo:cloudy", false},
		{"", false},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			if got := isCloudModel(tc.model); got != tc.want {
				t.Errorf("isCloudModel(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

// The envelope is not the only routing input any more: a cloud-hosted model
// cannot be served by the Mac at all, so naming one is a request for the cloud
// even when the client sends no routing fields — which is what an OpenAI-SDK
// agent does.
func TestRequiredCapabilityReadsModelLocality(t *testing.T) {
	cases := []struct {
		name         string
		privacy      string
		latencyClass string
		model        string
		want         string
	}{
		{"cloud-tagged model with no envelope", "", "", "deepseek-v4.1-flash:cloud", "cloud"},
		{"cloud-tagged model, unknown envelope", "LOCAL_PREFERRED", "", "minimax-m3:cloud", "cloud"},
		// Unchanged: the envelope still decides when the model says nothing.
		{"interactive goes to cloud", "", "interactive", "granite4:3b", "cloud"},
		{"no envelope defaults local", "", "", "granite4:3b", "local"},
		{"alias defaults local", "", "", "local-brain", "local"},
		{"alias on interactive goes cloud", "", "interactive", "local-brain", "cloud"},
		{"local model under LOCAL_ONLY", "LOCAL_ONLY", "", "granite4:3b", "local"},
		// LOCAL_ONLY outranks the latency class, as before.
		{"LOCAL_ONLY beats interactive", "LOCAL_ONLY", "interactive", "granite4:3b", "local"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := requiredCapability(tc.privacy, tc.latencyClass, tc.model)
			if err != nil {
				t.Fatalf("requiredCapability(%q, %q, %q) errored: %v", tc.privacy, tc.latencyClass, tc.model, err)
			}
			if got != tc.want {
				t.Errorf("requiredCapability(%q, %q, %q) = %q, want %q",
					tc.privacy, tc.latencyClass, tc.model, got, tc.want)
			}
		})
	}
}

// LOCAL_ONLY means the request never leaves the machine. A cloud model cannot be
// served locally, so the two are irreconcilable: the request must be refused,
// not quietly promoted to cloud and not quietly rerouted to a Mac that does not
// have the model.
func TestLocalOnlyRefusesACloudModelRatherThanPromotingIt(t *testing.T) {
	got, err := requiredCapability("LOCAL_ONLY", "", "deepseek-v4.1-flash:cloud")
	if err == nil {
		t.Fatal("LOCAL_ONLY with a cloud model was accepted, want a refusal")
	}
	if got != "" {
		t.Errorf("refusal returned capability %q, want \"\"", got)
	}
	// The refusal names the model, so the operator learns what was asked for
	// rather than seeing an anonymous conflict.
	if !strings.Contains(err.Error(), "deepseek-v4.1-flash:cloud") {
		t.Errorf("refusal does not name the model: %v", err)
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

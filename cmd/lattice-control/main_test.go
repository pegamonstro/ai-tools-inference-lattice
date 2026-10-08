package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
	// empty Models (not yet announced) hosts nothing — fail closed, not wildcard
	reg["mac"] = gw("mac", []string{"local", "chat"}, nil, 1, 0)
	if _, err := selectGateway("local", "granite3-moe:3b", reg, map[string]bool{"mac": true}); err == nil {
		t.Fatal("expected no match for a local gateway that announced no models")
	}
	// capability mismatch: a gateway lacking the required capability is filtered
	reg["mac"] = gw("mac", []string{"cloud"}, []string{"granite3-moe:3b"}, 1, 0)
	if _, err := selectGateway("local", "granite3-moe:3b", reg, map[string]bool{"mac": true}); err == nil {
		t.Fatal("expected no match for a gateway without the required capability")
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

func TestCapabilitiesListsConcreteModels(t *testing.T) {
	origCaps := capabilities
	origGateways := gateways
	t.Cleanup(func() { capabilities = origCaps; gateways = origGateways })

	capabilities = map[string]struct {
		local string
		cloud string
	}{
		"local-brain": {local: "gemma4:12b", cloud: "gemma4:31b-cloud"},
		"local-coder": {local: "qwen2.5-coder:3b", cloud: "deepseek-v4-pro:cloud"},
	}
	gateways = map[string]Gateway{
		"remote-gpu": {
			ID: "remote-gpu", Endpoint: "http://remote:8081",
			Capabilities: []string{"local", "chat", "image_generation", "speech"},
			Models:       []string{"gemma4:12b", "qwen3.8:27b", "whisper-small", "z-image-turbo", "sdxl-base"},
			ImageModels:  []string{"z-image-turbo", "sdxl-base"},
			Slots:        1, MaxContext: 262144,
		},
		"local": {
			ID: "local", Endpoint: "http://127.0.0.1:8081",
			Capabilities: []string{"local", "chat"},
			Models:       []string{"gemma4:12b", "bge-m3:latest"},
			Slots:        1, MaxContext: 131072,
		},
	}

	w := httptest.NewRecorder()
	handleCapabilities(w, httptest.NewRequest(http.MethodGet, "/capabilities", nil))

	var body struct {
		Capabilities []struct {
			ID string `json:"id"`
		} `json:"capabilities"`
		ImageModels []string `json:"image_models"`
		Models      []string `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode capabilities response: %v", err)
	}

	// Concrete names aggregate across gateways (gemma4 on both), keep the
	// speech + embedding entries, and come out deduplicated and sorted.
	want := []string{"bge-m3:latest", "gemma4:12b", "qwen3.8:27b", "whisper-small"}
	if len(body.Models) != len(want) {
		t.Fatalf("models = %v, want %v", body.Models, want)
	}
	for i := range want {
		if body.Models[i] != want[i] {
			t.Fatalf("models = %v, want %v", body.Models, want)
		}
	}

	// Names the client already has other listings for stay out: the aliases and
	// the image zoo (they would double-list on the same screen).
	for _, name := range body.Models {
		if name == "local-brain" {
			t.Fatal("alias id leaked into the concrete models list")
		}
		if name == "z-image-turbo" || name == "sdxl-base" {
			t.Fatalf("image model %s leaked into the concrete models list", name)
		}
	}
	if len(body.ImageModels) != 2 {
		t.Fatalf("image_models = %v, want the 2 image names", body.ImageModels)
	}
	foundAlias := false
	for _, c := range body.Capabilities {
		if c.ID == "local-brain" {
			foundAlias = true
		}
	}
	if !foundAlias {
		t.Fatal("capabilities list lost the aliases")
	}
}

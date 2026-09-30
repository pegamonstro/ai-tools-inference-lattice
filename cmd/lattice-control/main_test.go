package main

import "testing"

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

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

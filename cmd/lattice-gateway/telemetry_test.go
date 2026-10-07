package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// The gateway is the local executor, so its locality is a constant. It is set
// inside logTelemetry rather than at each call site: there are several call sites,
// and a field one of them forgets is a field that vanishes from the split
// without anyone noticing, because the line still looks complete.
func TestLogTelemetryStampsLocalLocality(t *testing.T) {
	path := t.TempDir() + "/telemetry-gateway.jsonl"
	t.Setenv("LATTICE_GATEWAY_TELEMETRY", path)

	logTelemetry(Telemetry{RequestID: "req-1", Model: "granite4:3b"}, "caller-set-in-test")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("logTelemetry wrote no line: %v", err)
	}
	var got Telemetry
	if err := json.Unmarshal(bytes.TrimSpace(raw), &got); err != nil {
		t.Fatalf("telemetry is not one JSON line: %v (%s)", err, raw)
	}
	if got.Locality != "local" {
		t.Errorf("locality = %q, want local", got.Locality)
	}
	// The stamp is additive: the caller's own fields must survive it.
	if got.RequestID != "req-1" || got.Model != "granite4:3b" {
		t.Errorf("the caller's fields were clobbered: %+v", got)
	}
}

package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsEndpoint(t *testing.T) {
	metrics.observe(Telemetry{Model: "flux-dev", Elapsed: 1.5})
	metrics.observe(Telemetry{Model: "flux-dev", Elapsed: 0.25, Error: "memory_pressure"})
	rr := httptest.NewRecorder()
	handleMetrics(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	for _, want := range []string{
		"lattice_gateway_requests_total",
		"lattice_gateway_request_duration_seconds",
		"lattice_gateway_queue_depth",
		"lattice_gateway_slots_limit",
		"lattice_gateway_errors_total",
		`lattice_gateway_errors_total{reason="memory_pressure"}`,
		"lattice_gateway_resident_bytes",
		"lattice_gateway_wired_estimate_bytes",
		"lattice_gateway_image_inflight",
		"lattice_gateway_provider_inflight",
		"lattice_gateway_provider_queue_depth",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics missing %q", want)
		}
	}
}

// gaugeLine returns the single line reporting name, so exact-value assertions
// cannot be satisfied by a longer number sharing a prefix.
func gaugeLine(t *testing.T, body, name string) string {
	t.Helper()
	var found []string
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, name+" ") || strings.HasPrefix(ln, name+"{") {
			found = append(found, ln)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %s line in /metrics, got %v", name, found)
	}
	return found[0]
}

// The live-state gauges must read the same numbers the wired gate enforces:
// ollama residency from the poller snapshot, the estimate at exactly
// residency + booked footprint, the booked count, and per-provider slot state.
// A scrape while one hold and one waiter are live pins both provider gauges.
func TestMetricsResidencyGauges(t *testing.T) {
	t.Setenv("LATTICE_GATEWAY_WIRED_LIMIT_MB", "0")
	const gib = 1024 * 1024 * 1024
	setPSResidentForTest(t, 15*gib)
	if !imageWiredLedger.tryEnter(8 * gib) {
		t.Fatal("booking refused")
	}
	defer imageWiredLedger.exit(8 * gib)

	s := renderSlotFor("metrics-probe")
	granted, err := s.acquire(context.Background(), 0)
	if err != nil || !granted {
		t.Fatalf("holder acquire: granted=%v err=%v", granted, err)
	}
	defer s.release()
	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan struct{})
	go func() {
		s.acquire(ctx, 0)
		close(waiterDone)
	}()
	time.Sleep(100 * time.Millisecond) // waiter is queued while the hold runs

	rr := httptest.NewRecorder()
	handleMetrics(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()

	if got := gaugeLine(t, body, "lattice_gateway_resident_bytes"); got != `lattice_gateway_resident_bytes 16106127360` {
		t.Errorf("resident gauge: %q", got)
	}
	if got := gaugeLine(t, body, "lattice_gateway_wired_estimate_bytes"); got != `lattice_gateway_wired_estimate_bytes 24696061952` {
		t.Errorf("wired estimate gauge: %q", got)
	}
	if got := gaugeLine(t, body, "lattice_gateway_image_inflight"); got != `lattice_gateway_image_inflight 1` {
		t.Errorf("image inflight gauge: %q", got)
	}
	// Labeled lookups include the label so entries other tests left in the
	// renderSlots map cannot confuse the count.
	if got := gaugeLine(t, body, `lattice_gateway_provider_inflight{provider="metrics-probe"}`); got != `lattice_gateway_provider_inflight{provider="metrics-probe"} 1` {
		t.Errorf("provider inflight gauge: %q", got)
	}
	if got := gaugeLine(t, body, `lattice_gateway_provider_queue_depth{provider="metrics-probe"}`); got != `lattice_gateway_provider_queue_depth{provider="metrics-probe"} 1` {
		t.Errorf("provider queue depth gauge: %q", got)
	}

	cancel()
	<-waiterDone
}

package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsEndpoint(t *testing.T) {
	metrics.observe(Telemetry{Model: "flux-dev", Elapsed: 1.5})
	rr := httptest.NewRecorder()
	handleMetrics(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	for _, want := range []string{
		"lattice_gateway_requests_total",
		"lattice_gateway_request_duration_seconds",
		"lattice_gateway_queue_depth",
		"lattice_gateway_slots_limit",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics missing %q", want)
		}
	}
}

package main

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
)

// metrics holds process-wide counters/gauges, updated from logTelemetry and
// served as Prometheus text format at /metrics.
var metrics = newMetrics()

type metricsStore struct {
	mu            sync.Mutex
	requestsTotal map[string]int64 // keyed by model
	errorsTotal   map[string]int64 // keyed by error reason
	durationSum   float64
	durationCount int64
}

func newMetrics() *metricsStore {
	return &metricsStore{
		requestsTotal: make(map[string]int64),
		errorsTotal:   make(map[string]int64),
	}
}

func (m *metricsStore) observe(te Telemetry) {
	m.mu.Lock()
	m.requestsTotal[te.Model]++
	m.durationSum += te.Elapsed
	m.durationCount++
	if te.Error != "" {
		m.errorsTotal[te.Error]++
	}
	m.mu.Unlock()
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	m := metrics
	m.mu.Lock()
	defer m.mu.Unlock()

	fmt.Fprintf(w, "# TYPE lattice_gateway_requests_total counter\n")
	for _, model := range sortedKeys(m.requestsTotal) {
		fmt.Fprintf(w, "lattice_gateway_requests_total{model=%q} %d\n", model, m.requestsTotal[model])
	}
	fmt.Fprintf(w, "# TYPE lattice_gateway_request_duration_seconds summary\n")
	if m.durationCount > 0 {
		fmt.Fprintf(w, "lattice_gateway_request_duration_seconds_sum %f\n", m.durationSum)
		fmt.Fprintf(w, "lattice_gateway_request_duration_seconds_count %d\n", m.durationCount)
	}
	fmt.Fprintf(w, "# TYPE lattice_gateway_queue_depth gauge\n")
	fmt.Fprintf(w, "lattice_gateway_queue_depth %d\n", inferenceSlots.waiting())
	fmt.Fprintf(w, "# TYPE lattice_gateway_slots_limit gauge\n")
	fmt.Fprintf(w, "lattice_gateway_slots_limit %d\n", inferenceSlots.limitValue())
}

func sortedKeys(m map[string]int64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

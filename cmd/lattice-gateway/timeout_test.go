package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The timeout must be generous by default: a default request asks for up to 4096
// output tokens, and this Mac decodes at roughly 6.7 tok/s, so the full default
// output alone needs ~10 minutes, before prefill is counted. A bound tighter than
// that turns ordinary work into a 500. It must still be overridable, because a
// busier or lighter host needs a different number.
func TestOllamaTimeoutDefaultsGenerouslyAndHonoursOverride(t *testing.T) {
	if got := ollamaTimeout(); got != 20*time.Minute {
		t.Errorf("default timeout = %v, want 20m", got)
	}

	t.Setenv("LATTICE_GATEWAY_OLLAMA_TIMEOUT", "90s")
	if got := ollamaTimeout(); got != 90*time.Second {
		t.Errorf("override timeout = %v, want 90s", got)
	}

	// An unparseable value must fall back rather than yield a zero timeout, which
	// would make every request fail instantly.
	t.Setenv("LATTICE_GATEWAY_OLLAMA_TIMEOUT", "banana")
	if got := ollamaTimeout(); got != 20*time.Minute {
		t.Errorf("invalid value gave %v, want the 20m default", got)
	}

	t.Setenv("LATTICE_GATEWAY_OLLAMA_TIMEOUT", "-5s")
	if got := ollamaTimeout(); got != 20*time.Minute {
		t.Errorf("negative value gave %v, want the 20m default", got)
	}
}

// A caller that gave up while queued must not take the inference slot; on a
// single-slot machine that would block everyone behind an abandoned request.
func TestAcquireSlotYieldsToCancelledContext(t *testing.T) {
	// Fill the only slot.
	inferenceSlots <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if acquireSlot(ctx) {
		<-inferenceSlots
		t.Fatal("acquireSlot took the slot for a cancelled caller")
	}

	// Free the slot and confirm a live caller still gets it.
	<-inferenceSlots
	if !acquireSlot(context.Background()) {
		t.Fatal("acquireSlot refused a live caller with a free slot")
	}
	<-inferenceSlots
}

// The timeout is a real bound: a server that never answers must produce an error,
// not a hang.
func TestExecuteReturnsWhenTimeoutElapses(t *testing.T) {
	t.Setenv("LATTICE_GATEWAY_OLLAMA_TIMEOUT", "50ms")

	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond)
	}))
	defer ollama.Close()

	p := &OllamaProvider{Endpoint: ollama.URL}
	start := time.Now()
	_, err := p.Execute(context.Background(), Request{Model: "hermes3:8b", Messages: []interface{}{}})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error, got none")
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("Execute took %v; the 50ms bound did not bite", elapsed)
	}
}

// A disconnected client must release the in-flight inference: the gateway's whole
// streaming cancellation story depends on the caller's context reaching Ollama.
func TestExecuteStopsWhenCallerContextIsCancelled(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond)
	}))
	defer ollama.Close()

	p := &OllamaProvider{Endpoint: ollama.URL}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := p.Execute(ctx, Request{Model: "hermes3:8b", Messages: []interface{}{}})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected cancellation to surface as an error")
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("Execute ran %v past cancellation", elapsed)
	}
}

// Streaming bounds time-to-first-byte, not total duration: a server that never
// starts responding is caught, so a dead Ollama cannot pin the inference slot.
func TestStreamBoundsTimeToFirstByte(t *testing.T) {
	t.Setenv("LATTICE_GATEWAY_OLLAMA_TIMEOUT", "50ms")

	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond)
	}))
	defer ollama.Close()

	p := &OllamaProvider{Endpoint: ollama.URL}
	start := time.Now()
	_, _, err := p.ExecuteStream(context.Background(), Request{Model: "hermes3:8b", Messages: []interface{}{}}, httptest.NewRecorder())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a stalled stream to error")
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("ExecuteStream hung %v before failing", elapsed)
	}
}

// The mirror of the previous test: headers arrive at once, so a long answer must
// be allowed to stream well past the header bound. This is the property the old
// comment claimed and the code did not have.
func TestStreamAllowsBodyToOutliveHeaderBound(t *testing.T) {
	t.Setenv("LATTICE_GATEWAY_OLLAMA_TIMEOUT", "50ms")

	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// First token, then a stall longer than the header bound, then the terminal event.
		w.Write([]byte(`{"model":"hermes3:8b","message":{"content":"hel"}}` + "\n"))
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(`{"model":"hermes3:8b","message":{"content":"lo"},"prompt_eval_count":3,"eval_count":2,"done":true}` + "\n"))
	}))
	defer ollama.Close()

	p := &OllamaProvider{Endpoint: ollama.URL}
	res, committed, err := p.ExecuteStream(context.Background(), Request{Model: "hermes3:8b", Messages: []interface{}{}}, httptest.NewRecorder())
	if err != nil {
		t.Fatalf("a slow-but-live stream was killed: %v", err)
	}
	if !committed {
		t.Error("stream was not committed after headers were sent")
	}
	if res == nil || res.Usage.CompletionTokens != 2 {
		t.Errorf("stream did not complete: %+v", res)
	}
}

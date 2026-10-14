package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// imageHarness installs the image-test globals and disables the memory + wired
// admission checks (margins of 1, wired off) so the tests observe the
// serialization layer in isolation. It also raises the global inference
// ceiling to 4: a roomy host is the premise — same-provider serialization is
// the provider slot's job, not the global slot's.
func imageHarness(t *testing.T, provs map[string]Provider, routes map[string]string) {
	t.Helper()
	t.Setenv("LATTICE_GATEWAY_IMAGE_MARGIN_MB", "1")
	t.Setenv("LATTICE_GATEWAY_WIRED_LIMIT_MB", "0")

	oldBudgeter, oldProviders, oldRegistry := budgeter, providers, registry
	budgeter = &MemoryBudgeter{safeMargin: 0, pageSize: 4096}
	providers = provs
	registry = &providerRegistry{modelProviders: routes}
	oldLimit := inferenceSlots.limitValue()
	inferenceSlots.setLimit(4)
	t.Cleanup(func() {
		inferenceSlots.setLimit(oldLimit)
		budgeter, providers, registry = oldBudgeter, oldProviders, oldRegistry
	})
}

// countingSidecar serves the image body and reports the peak concurrency its
// handler saw.
func countingSidecar(t *testing.T, hold time.Duration) (string, *int64) {
	t.Helper()
	var counts struct{ cur, peak int64 }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&counts.cur, 1)
		for {
			p := atomic.LoadInt64(&counts.peak)
			if n <= p || atomic.CompareAndSwapInt64(&counts.peak, p, n) {
				break
			}
		}
		time.Sleep(hold)
		atomic.AddInt64(&counts.cur, -1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image":"aW1hZ2U=","seed":1,"width":512,"height":512,"seconds":1.0}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &counts.peak
}

// slowSidecar serves the image body after a fixed hold.
func slowSidecar(t *testing.T, hold time.Duration) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(hold)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image":"aW1hZ2U=","seed":1,"width":512,"height":512,"seconds":1.0}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func fireImage(wg *sync.WaitGroup, code *int, model string) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest("POST", "/v1/images/generations",
			strings.NewReader(`{"model":"`+model+`","prompt":"a cat","size":"512x512","n":1}`))
		rec := httptest.NewRecorder()
		handleImageGenerations(rec, req)
		*code = rec.Code
	}()
}

// Two concurrent renders on the same provider must not overlap: the sidecars
// are cold-Popen CLIs, so two sd-cli children on one checkpoint would contend
// for the same model load and VRAM. Both requests still succeed (queue, not
// reject).
func TestSameProviderSerializes(t *testing.T) {
	url, peak := countingSidecar(t, 150*time.Millisecond)
	imageHarness(t,
		map[string]Provider{"mflux": &MfluxProvider{Endpoint: url, name: "mflux"}},
		map[string]string{"flux-dev": "mflux"})

	var codes [2]int
	var wg sync.WaitGroup
	fireImage(&wg, &codes[0], "flux-dev")
	fireImage(&wg, &codes[1], "flux-dev")
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, c)
		}
	}
	if got := atomic.LoadInt64(peak); got != 1 {
		t.Fatalf("sidecar saw %d concurrent renders, want serialized (1)", got)
	}
}

// Distinct providers may render in parallel — four render CLIs sit behind
// different engines, and the global inference ceiling is what bounds their sum.
func TestDifferentProvidersParallel(t *testing.T) {
	url, peak := countingSidecar(t, 150*time.Millisecond)
	imageHarness(t,
		map[string]Provider{
			"mflux": &MfluxProvider{Endpoint: url, name: "mflux"},
			"sdxl":  &MfluxProvider{Endpoint: url, name: "sdxl"},
		},
		map[string]string{"flux-dev": "mflux", "sdxl-ckpt": "sdxl"})

	var codes [2]int
	var wg sync.WaitGroup
	fireImage(&wg, &codes[0], "flux-dev")
	fireImage(&wg, &codes[1], "sdxl-ckpt")
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, c)
		}
	}
	if got := atomic.LoadInt64(peak); got != 2 {
		t.Fatalf("sidecar saw %d concurrent renders, want parallel (2)", got)
	}
}

// Beyond one running + the queue depth, same-provider requests are rejected
// with 429 — the user-facing backpressure — and neither the provider slot nor
// a wired booking leaks.
func TestProviderQueueFull429(t *testing.T) {
	slow := slowSidecar(t, 250*time.Millisecond)
	imageHarness(t,
		map[string]Provider{"sdxl": &MfluxProvider{Endpoint: slow, name: "sdxl"}},
		map[string]string{"sdxl-ckpt": "sdxl"})

	var codes [4]int
	var wg sync.WaitGroup
	for i := range codes {
		fireImage(&wg, &codes[i], "sdxl-ckpt")
	}
	wg.Wait()

	full := 0
	for i, c := range codes {
		switch c {
		case http.StatusOK:
		case http.StatusTooManyRequests:
			full++
		default:
			t.Fatalf("request %d: unexpected status %d", i, c)
		}
	}
	if full != 1 {
		t.Fatalf("got %d rejections, want exactly 1 (1 running + queue 2 of 4 concurrent)", full)
	}

	// No leaks: the provider slot is free again and no wired booking remains.
	if _, count := imageWiredLedger.inFlight(); count != 0 {
		t.Fatalf("wired ledger leaked %d bookings after the burst", count)
	}
	req := httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"sdxl-ckpt","prompt":"a cat","size":"512x512","n":1}`))
	rec := httptest.NewRecorder()
	handleImageGenerations(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("post-burst request: status = %d, want 200 (slots leaked)", rec.Code)
	}
}

// A client that disconnects while queued on a busy provider returns without a
// render and without holding either slot, telemetry-tagged the same way the
// global-slot cancel path is.
func TestImageCancelWhileQueuedReleasesSlots(t *testing.T) {
	slow := slowSidecar(t, 300*time.Millisecond)
	imageHarness(t,
		map[string]Provider{"sdxl": &MfluxProvider{Endpoint: slow, name: "sdxl"}},
		map[string]string{"sdxl-ckpt": "sdxl"})

	var aCode, bCode int
	var wg sync.WaitGroup

	// A takes the provider slot.
	fireImage(&wg, &aCode, "sdxl-ckpt")
	time.Sleep(60 * time.Millisecond) // A now holds a global slot and the provider slot

	// B queues behind A on the provider slot, then the client walks away.
	ctx, cancel := context.WithCancel(context.Background())
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest("POST", "/v1/images/generations",
			strings.NewReader(`{"model":"sdxl-ckpt","prompt":"a cat","size":"512x512","n":1}`)).WithContext(ctx)
		rec := httptest.NewRecorder()
		handleImageGenerations(rec, req)
		bCode = rec.Code
	}()
	time.Sleep(60 * time.Millisecond) // B is now waiting on the provider slot
	cancel()
	wg.Wait()

	// A cancelled connection yields an unwritten recorder: default 200, empty body.
	if bCode != http.StatusOK {
		t.Fatalf("cancelled request: recorder status %d, want the untouched 200", bCode)
	}
	if aCode != http.StatusOK {
		t.Fatalf("holder request: status = %d, want 200", aCode)
	}

	if _, count := imageWiredLedger.inFlight(); count != 0 {
		t.Fatalf("wired ledger leaked %d bookings after cancellation", count)
	}
}

// resolveProviderQueue maps the env knob onto a queue depth: unset, invalid,
// and <1 all hold the default — the value is a safety limiter, and an
// operator typo must not open the queue.
func TestProviderQueueMax(t *testing.T) {
	cases := map[string]int{
		"":   2,
		"0":  2,
		"-3": 2,
		"x":  2,
		"1":  1,
		"5":  5,
	}
	for in, want := range cases {
		if got := resolveProviderQueue(in); got != want {
			t.Errorf("resolveProviderQueue(%q) = %d, want %d", in, got, want)
		}
	}
}

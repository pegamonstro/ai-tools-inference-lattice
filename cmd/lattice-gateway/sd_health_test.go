package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// sdHealthHarness swaps the provider registry and resets the package-global
// sdxl /health discovery cache, in imageHarness's save+restore style.
func sdHealthHarness(t *testing.T, provs map[string]Provider) {
	t.Helper()
	oldProviders := providers
	providers = provs
	sdHealthMu.Lock()
	oldHealth := sdHealth
	sdHealth = sdHealthState{}
	sdHealthMu.Unlock()
	t.Cleanup(func() {
		providers = oldProviders
		sdHealthMu.Lock()
		sdHealth = oldHealth
		sdHealthMu.Unlock()
	})
}

// The sidecar publishes its own conditioning paths at GET /health; discovery
// must read that truth (not the gateway env mirror) so footprint sizing keeps
// matching the files the sidecar actually loads.
func TestSdConditioningDiscoveryFromHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("discovery fetched %s", r.URL.Path)
		}
		w.Write([]byte(`{"status":"ok","ckpt_dir":"/sc","control_dir":"/sc/control","ip_adapter":"/sc/ip-adapter.safetensors","clip_vision":"/sc/clip_vision/model.safetensors"}`))
	}))
	t.Cleanup(srv.Close)
	// The registry key is the provider name; only the KIND string marks the
	// sd.cpp engine, so a non-"sdxl" name with kind "sdxl" must still be found.
	sdHealthHarness(t, map[string]Provider{
		"checkpoint-engine": &MfluxProvider{Endpoint: srv.URL, name: "sdxl"},
	})
	// A distinctive env mirror proves discovery wins over env even when env is set.
	t.Setenv("SD_CKPT_DIR", "/env")
	t.Setenv("SD_CONTROL_MODEL_DIR", "/env/control")
	t.Setenv("SD_IPADAPTER_PATH", "/env/ip-adapter.safetensors")
	t.Setenv("SD_CLIP_VISION_PATH", "/env/clip_vision/model.safetensors")

	pollSdConditioning()
	controlDir, ipadapter, clipVision := sdConditioningPaths()
	if controlDir != "/sc/control" || ipadapter != "/sc/ip-adapter.safetensors" || clipVision != "/sc/clip_vision/model.safetensors" {
		t.Fatalf("discovery lost to env mirror: got %q %q %q", controlDir, ipadapter, clipVision)
	}
}

// A /health body without ckpt_dir is not accepted: the gateway keeps the env
// mirror rather than bookkeeping half a truth.
func TestSdConditioningMissingCkptRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","control_dir":"/sc/control"}`))
	}))
	t.Cleanup(srv.Close)
	sdHealthHarness(t, map[string]Provider{"sdxl": &MfluxProvider{Endpoint: srv.URL, name: "sdxl"}})
	t.Setenv("SD_CKPT_DIR", "/env")

	pollSdConditioning()
	controlDir, _, _ := sdConditioningPaths()
	if controlDir != "/env/control" {
		t.Fatalf("missing ckpt_dir must not seed discovery; got %q", controlDir)
	}
}

// Poll failures keep the last-known paths (same doctrine as the /api/ps
// snapshot) instead of dropping back to the mirror mid-run.
func TestSdConditioningRetainedOnFailure(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"status":"ok","ckpt_dir":"/sc","control_dir":"/sc/control"}`))
	}))
	t.Cleanup(srv.Close)
	sdHealthHarness(t, map[string]Provider{"sdxl": &MfluxProvider{Endpoint: srv.URL, name: "sdxl"}})
	t.Setenv("SD_CKPT_DIR", "/env")

	pollSdConditioning()
	up.Store(false)
	pollSdConditioning()
	controlDir, _, _ := sdConditioningPaths()
	if controlDir != "/sc/control" {
		t.Fatalf("failed poll must retain last-known paths; got %q", controlDir)
	}
}

// Without a kind-"sdxl" provider (or with no cache yet) the env mirror rules —
// the discovery layer is additive, not a dependency.
func TestSdConditioningFallbackMirror(t *testing.T) {
	sdHealthHarness(t, map[string]Provider{"flux": &MfluxProvider{Endpoint: "http://unused", name: "mflux"}})
	t.Setenv("SD_CKPT_DIR", "/env")
	t.Setenv("SD_CONTROL_MODEL_DIR", "")
	t.Setenv("SD_IPADAPTER_PATH", "/env/ip.safetensors")
	t.Setenv("SD_CLIP_VISION_PATH", "")

	pollSdConditioning()
	controlDir, ipadapter, clipVision := sdConditioningPaths()
	if controlDir != "/env/control" || ipadapter != "/env/ip.safetensors" || clipVision != "/env/clip_vision/model.safetensors" {
		t.Fatalf("mirror fallback drifted: %q %q %q", controlDir, ipadapter, clipVision)
	}
}

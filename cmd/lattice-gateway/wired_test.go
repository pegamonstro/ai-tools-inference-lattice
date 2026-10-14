package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// wiredFits is the pure math behind the wired-memory ledger: a render's
// footprint fits when the ollama residency the /api/ps poller saw, plus what
// in-flight renders already booked, plus this footprint, plus the safety gap,
// all stay inside the host's iogpu wired limit. The gap is the reserve that
// keeps the ledger conservative — it is counted even when the footprint is
// zero, because gap is protecting OS+non-GPU headroom, not the render.
func TestWiredFits(t *testing.T) {
	const mb = 1024 * 1024
	cases := []struct {
		name                                 string
		limit, resident, inflight, foot, gap uint64
		want                                 bool
	}{
		{"empty host fits", 24576 * mb, 0, 0, 8192 * mb, 2048 * mb, true},
		{"exact fit equality passes", 10, 5, 2, 2, 1, true},
		{"one byte over fails", 10, 5, 2, 3, 1, false},
		{"gap reserves even at zero footprint", 10, 9, 0, 0, 2, false},
		{"limit zero never fits", 0, 0, 0, 1, 0, false},
		{"footprint alone exceeds", 24, 0, 0, 25, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wiredFits(tc.limit, tc.resident, tc.inflight, tc.foot, tc.gap)
			if got != tc.want {
				t.Fatalf("wiredFits(%d,%d,%d,%d,%d) = %v, want %v", tc.limit, tc.resident, tc.inflight, tc.foot, tc.gap, got, tc.want)
			}
		})
	}
}

// setPSResidentForTest writes the ps residency snapshot the ledger consults,
// the same global the poller publishes.
func setPSResidentForTest(t *testing.T, bytes uint64) {
	t.Helper()
	psCacheMu.Lock()
	psCache.residentBytes = bytes
	psCacheMu.Unlock()
}

// TestWiredLedgerAtomic hammers tryEnter concurrently against a 24 MiB budget
// and 10 MiB footprints: exactly two may enter no matter how many race for
// them, a freed slot is reusable, and paired exits land the ledger back at a
// clean zero (no byte or count leak, no underflow).
func TestWiredLedgerAtomic(t *testing.T) {
	t.Setenv("LATTICE_GATEWAY_WIRED_LIMIT_MB", "24")
	t.Setenv("LATTICE_GATEWAY_WIRED_GAP_MB", "0")
	setPSResidentForTest(t, 0)

	const foot = 10 << 20
	var ledger wiredLedger
	var accepted, rejected int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ledger.tryEnter(foot) {
				atomic.AddInt64(&accepted, 1)
			} else {
				atomic.AddInt64(&rejected, 1)
			}
		}()
	}
	wg.Wait()

	if accepted != 2 || rejected != 48 {
		t.Fatalf("accepted=%d rejected=%d, want 2/48 (24 MiB budget, 10 MiB footprints)", atomic.LoadInt64(&accepted), atomic.LoadInt64(&rejected))
	}
	b, c := ledger.inFlight()
	if b != 2*foot || c != 2 {
		t.Fatalf("after race: bytes=%d count=%d, want %d/2", b, c, 2*foot)
	}

	ledger.exit(foot)
	if !ledger.tryEnter(foot) {
		t.Fatal("freed slot not reusable")
	}
	ledger.exit(foot)
	ledger.exit(foot)

	b, c = ledger.inFlight()
	if b != 0 || c != 0 {
		t.Fatalf("ledger not clean after paired exits: bytes=%d count=%d, want 0/0", b, c)
	}
}

// An unset-zero limit means the feature is off (non-Mac hosts sysctl-fail to
// zero): tryEnter must admit unconditionally and never book bytes that would
// later reject.
func TestWiredLedgerDisabled(t *testing.T) {
	t.Setenv("LATTICE_GATEWAY_WIRED_LIMIT_MB", "0")
	if wiredLimitBytes() != 0 {
		t.Fatalf("wiredLimitBytes() = %d, want 0 (disabled)", wiredLimitBytes())
	}
	var ledger wiredLedger
	if !ledger.tryEnter(1 << 30) {
		t.Fatal("disabled ledger rejected a render")
	}
	if _, c := ledger.inFlight(); c != 1 {
		t.Fatalf("inFlight count = %d, want 1 (disabled ledger still tracks)", c)
	}
	ledger.exit(1 << 30)
}

// Residency observed by the /api/ps poller must count against the budget even
// with an empty ledger — the ollama collision is the whole failure mode this
// gate exists for.
func TestWiredLedgerResidentFloor(t *testing.T) {
	t.Setenv("LATTICE_GATEWAY_WIRED_LIMIT_MB", "24")
	t.Setenv("LATTICE_GATEWAY_WIRED_GAP_MB", "0")
	setPSResidentForTest(t, 15<<20)

	var ledger wiredLedger
	if !ledger.tryEnter(8 << 20) {
		t.Fatal("first footprint (resident 15+8=23 of 24 MiB) rejected")
	}
	if ledger.tryEnter(8 << 20) {
		t.Fatal("second footprint (resident 15+ledger 8+8=31 of 24 MiB) accepted")
	}
	ledger.exit(8 << 20)
}

// The footprint estimator: stat the sidecar model path when it names a regular
// file; fall back to 8 GiB when it names a missing file or an empty model
// (the deployed providers.json carries no upstream mappings, so an empty
// SidecarModel is the common path, and 8 GiB brackets SDXL fp16 6.9 and
// FLUX 4-bit 6.8). Conditioning rides generate requests only — mirror the
// providers.go relay rule — and each referenced conditioning file is added by
// its own size, with a missing file contributing zero (the sidecar would fail
// loudly on it anyway, so an optimism there never coincides with a render).
func TestImageFootprint(t *testing.T) {
	dir := t.TempDir()
	ckpt := filepath.Join(dir, "sdxl")
	sparse := func(rel string, sz int64) string {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := f.Truncate(sz); err != nil {
			t.Fatal(err)
		}
		return p
	}
	model := sparse("model.safetensors", 6<<20)
	// Conditioning files are resolved from the SD_CKPT_DIR mirror by name;
	// create them for side effect only.
	sparse(filepath.Join("sdxl", "control", "pose.safetensors"), 5<<20)
	sparse(filepath.Join("sdxl", "ip-adapter.safetensors"), 700<<10)
	sparse(filepath.Join("sdxl", "clip_vision", "model.safetensors"), 2528<<10)
	t.Setenv("SD_CKPT_DIR", ckpt)

	const mb = 1024 * 1024
	const fb = fallbackRenderFootprint
	cases := []struct {
		name string
		op   string
		img  ImageRequest
		want uint64
	}{
		{"stat'd model wins", "generate", ImageRequest{SidecarModel: model}, 6 * mb},
		{"missing path falls back", "generate", ImageRequest{SidecarModel: filepath.Join(dir, "nope.safetensors")}, fb},
		{"empty model falls back", "generate", ImageRequest{}, fb},
		{"directory falls back", "generate", ImageRequest{SidecarModel: dir}, fb},
		{"conditioning added on generate", "generate",
			ImageRequest{SidecarModel: model, Extension: &ImageExtension{Control: &ImageControl{Net: "pose"}}}, 6*mb + 5*mb},
		{"ip-adapter adds adapter and clip vision", "generate",
			ImageRequest{SidecarModel: model, Extension: &ImageExtension{IPAdapter: &ImageIPAdapter{Image: "aQ=="}}}, 6*mb + 700*1024 + 2528*1024},
		{"both conditionings stack", "generate",
			ImageRequest{SidecarModel: model, Extension: &ImageExtension{
				Control:   &ImageControl{Net: "pose"},
				IPAdapter: &ImageIPAdapter{Image: "aQ=="}}}, 6*mb + 5*mb + 700*1024 + 2528*1024},
		{"missing conditioning net adds zero", "generate",
			ImageRequest{SidecarModel: model, Extension: &ImageExtension{Control: &ImageControl{Net: "canny"}}}, 6 * mb},
		{"edit never counts conditioning", "edit",
			ImageRequest{SidecarModel: model, Extension: &ImageExtension{Control: &ImageControl{Net: "pose"}}}, 6 * mb},
		{"nil extension is bare base", "generate", ImageRequest{SidecarModel: model}, 6 * mb},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageFootprint(tc.op, tc.img); got != tc.want {
				t.Fatalf("imageFootprint(%q) = %s, want %s", tc.op, gb(got), gb(tc.want))
			}
		})
	}
}

// gb renders byte counts in readable MiB for failure output only.
func gb(b uint64) string { return fmt.Sprintf("%dMiB (0x%x)", b>>20, b) }

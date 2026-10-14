package main

// Wired-memory residency gate. The M6 host runs resident ollama llama-servers
// alongside per-request render CLIs against one unified-memory GPU, and
// iogpu.wired_limit_mb caps what Metal processes may pin. Round 1 made that
// survivable (wired 24576 + OLLAMA_KEEP_ALIVE=1m); this gate makes it counted:
// the gateway tracks ollama's resident VRAM from /api/ps and refuses an image
// render whose footprint would push residency — ollama plus booked in-flight
// renders — past the wired limit. It is accounting, not reservation: numbers
// are floors and the gap (2 GiB default) is the safety margin between them.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pegamonstro/ai-tools-inference-lattice/pkg/latticeconfig"
)

// fallbackRenderFootprint is the footprint claimed for a render whose model
// path the gateway cannot size. Deployed providers.json carries no upstream
// mappings, so an empty SidecarModel is the common path; and FLUX model values
// are directories (os.Stat sizes the directory entry, not the weights). 8 GiB
// brackets every routed engine today — SDXL fp16 6.9 and FLUX 4-bit 6.8.
const fallbackRenderFootprint = 8 * 1024 * 1024 * 1024

// psSnapshot is the most recent successful ollama /api/ps read: the summed
// size_vram of every resident model. On poll failure the snapshot is retained
// — a stale figure only ever over-counts, never under-protects.
type psSnapshot struct {
	residentBytes uint64
	fetchedAt     time.Time
}

var (
	psCacheMu       sync.Mutex
	psCache         psSnapshot
	psPollHealthy   bool
	ollamaPSFailed  = "ollama /api/ps poll failed"
	ollamaPSBackMsg = "ollama /api/ps poll recovered"
)

func ollamaPSResidentBytes() uint64 {
	psCacheMu.Lock()
	defer psCacheMu.Unlock()
	return psCache.residentBytes
}

// pollOllamaPS reads /api/ps once and sums the resident models' VRAM into the
// ps snapshot. Failures keep the last value and log only on state transitions,
// so a long ollama outage cannot spam the log every 5 seconds.
func pollOllamaPS() {
	fail := func(err error) {
		psCacheMu.Lock()
		was := psPollHealthy
		psPollHealthy = false
		psCacheMu.Unlock()
		if was {
			log.Printf("%s (keeping last residency %d bytes): %v", ollamaPSFailed, ollamaPSResidentBytes(), err)
		}
	}

	resp, err := http.Get(ollamaURL + "/api/ps")
	if err != nil {
		fail(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Errorf("/api/ps returned %d", resp.StatusCode))
		return
	}
	var ps struct {
		Models []struct {
			Name     string `json:"name"`
			SizeVRAM int64  `json:"size_vram"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ps); err != nil {
		fail(err)
		return
	}
	var total uint64
	for _, m := range ps.Models {
		if m.SizeVRAM > 0 {
			total += uint64(m.SizeVRAM)
		}
	}
	psCacheMu.Lock()
	psCache = psSnapshot{residentBytes: total, fetchedAt: time.Now()}
	if !psPollHealthy {
		psPollHealthy = true
		log.Printf("%s (resident %d bytes across %d models)", ollamaPSBackMsg, total, len(ps.Models))
	}
	psCacheMu.Unlock()
}

// goOllamaPSPoller runs the residency poll in the background for the process's
// lifetime: one immediate poll (so the first request sees real residency even
// before the first tick) then every 5 seconds — staleness bounded at the poll
// interval.
func goOllamaPSPoller() {
	go func() {
		pollOllamaPS()
		ticker := time.NewTicker(5 * time.Second)
		for range ticker.C {
			pollOllamaPS()
		}
	}()
}

var (
	wiredLimitOnce   sync.Once
	wiredLimitSysctl uint64
)

// wiredLimitBytes is the budget the ledger fills toward. LATTICE_GATEWAY_WIRED_LIMIT_MB
// wins when set (0 = feature off), else the host's iogpu.wired_limit_mb is read
// once — a sysctl call fails on Linux and CI, which safely disables the gate.
func wiredLimitBytes() uint64 {
	if v := latticeconfig.Env("LATTICE_GATEWAY_WIRED_LIMIT_MB", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return uint64(n) * 1024 * 1024
		}
	}
	wiredLimitOnce.Do(func() {
		out, err := exec.Command("sysctl", "-n", "iogpu.wired_limit_mb").Output()
		if err != nil {
			return // stays 0: gate off on hosts without iogpu
		}
		if n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64); err == nil {
			wiredLimitSysctl = n * 1024 * 1024
		}
	})
	return wiredLimitSysctl
}

// wiredGapBytes is the safety reserve held between the counted numbers and the
// wired limit: OS + non-GPU RAM (and any residency the poll has not yet seen).
// Default 2048 MiB, overridable via LATTICE_GATEWAY_WIRED_LIMIT_MB's sibling
// LATTICE_GATEWAY_WIRED_GAP_MB.
func wiredGapBytes() uint64 {
	mb := 2048
	if v := latticeconfig.Env("LATTICE_GATEWAY_WIRED_GAP_MB", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			mb = n
		}
	}
	return uint64(mb) * 1024 * 1024
}

// wiredFits is the gate's pure math, hoisted for table tests: the render fits
// when residency plus booked plus its footprint plus the gap stays inside the
// limit. The gap counts even at zero footprint — it protects headroom, not the
// render — and a zero limit fits nothing.
func wiredFits(limit, resident, inflight, footprint, gap uint64) bool {
	return limit > 0 && resident+inflight+footprint+gap <= limit
}

// wiredLedger books in-flight render footprints against the wired budget.
// tryEnter is an atomic check-and-add under one mutex; exit releases the
// caller's footprint. A disabled limit admits unconditionally but still
// tracks, so /metrics stays informative on hosts without the gate.
type wiredLedger struct {
	mu    sync.Mutex
	bytes uint64
	count int
}

func (w *wiredLedger) tryEnter(footprint uint64) bool {
	limit := wiredLimitBytes()
	if limit <= 0 {
		w.mu.Lock()
		w.bytes += footprint
		w.count++
		w.mu.Unlock()
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !wiredFits(limit, ollamaPSResidentBytes(), w.bytes, footprint, wiredGapBytes()) {
		return false
	}
	w.bytes += footprint
	w.count++
	return true
}

func (w *wiredLedger) exit(footprint uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if footprint > w.bytes {
		w.bytes = 0
	} else {
		w.bytes -= footprint
	}
	if w.count > 0 {
		w.count--
	}
}

func (w *wiredLedger) inFlight() (bytes uint64, count int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytes, w.count
}

// the image renders' footprint ledger (shared across providers — the budget
// is host-wide residency, not per sidecar).
var imageWiredLedger wiredLedger

// statBytes sizes a regular file for footprint accounting; 0 for anything else
// (missing path, directory). Directories must not size themselves, because a
// FLUX model value is a directory path and its stat size is meaningless.
func statBytes(path string) uint64 {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return 0
	}
	if fi.Size() <= 0 {
		return 0
	}
	return uint64(fi.Size())
}

// sdConditioningPaths mirrors deploy/sd-sidecar.py's conditioning defaults
// (SD_CKPT_DIR/SD_CONTROL_MODEL_DIR/SD_IPADAPTER_PATH/SD_CLIP_VISION_PATH).
// The sidecar resolves its checkpoint dir at startup; the gateway resolves the
// same defaults per request because the two processes are colocated on the
// same host and share one filesystem. If the operator overrides the sidecar
// env without mirroring it here, the mirror silently reads zero-byte files —
// an accepted accounting gap (the sidecar would fail loudly on a truly missing
// conditioning model long before residency could matter).
func sdConditioningPaths() (controlDir, ipadapter, clipVision string) {
	ckpt := os.Getenv("SD_CKPT_DIR")
	if ckpt == "" {
		home, _ := os.UserHomeDir()
		ckpt = filepath.Join(home, "mflux-models", "sdxl")
	}
	controlDir = os.Getenv("SD_CONTROL_MODEL_DIR")
	if controlDir == "" {
		controlDir = filepath.Join(ckpt, "control")
	}
	ipadapter = os.Getenv("SD_IPADAPTER_PATH")
	if ipadapter == "" {
		ipadapter = filepath.Join(ckpt, "ip-adapter.safetensors")
	}
	clipVision = os.Getenv("SD_CLIP_VISION_PATH")
	if clipVision == "" {
		clipVision = filepath.Join(ckpt, "clip_vision", "model.safetensors")
	}
	return controlDir, ipadapter, clipVision
}

// imageFootprint estimates the unified-memory footprint an image render will
// pin: the model file's size (falling back to 8 GiB when unsizable), plus the
// conditioning models a generate request references. Conditioning counts only
// on generate with a non-nil Extension — mirroring providers.go's relay rule
// that edit bodies never carry control_net/ip_adapter.
func imageFootprint(op string, img ImageRequest) uint64 {
	base := statBytes(img.SidecarModel)
	if base == 0 {
		base = fallbackRenderFootprint
	}
	if op != "generate" || img.Extension == nil {
		return base
	}
	controlDir, ipadapter, clipVision := sdConditioningPaths()
	if c := img.Extension.Control; c != nil {
		base += statBytes(filepath.Join(controlDir, c.Net+".safetensors"))
	}
	if img.Extension.IPAdapter != nil {
		base += statBytes(ipadapter)
		base += statBytes(clipVision)
	}
	return base
}

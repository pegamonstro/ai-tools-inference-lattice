package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeGateway is a minimal gateway /telemetry endpoint: a growing, unbounded
// ring of events addressed by seq, exactly the contract fetchGatewayTelemetry
// reads. Each event carries the boot id so recovery-by-boot is exercised.
type fakeGateway struct {
	mu     sync.Mutex
	boot   string
	events []string // raw JSON, each a positive "seq"
	server *httptest.Server
}

func newFakeGateway(t *testing.T, boot string) *fakeGateway {
	t.Helper()
	g := &fakeGateway{boot: boot}
	g.server = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeGateway) handle(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var since int64
	fmt.Sscanf(r.URL.Query().Get("since"), "%d", &since)

	events := make([]json.RawMessage, 0, len(g.events))
	var oldest, seq int64
	for _, raw := range g.events {
		var row struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			continue
		}
		if oldest == 0 || row.Seq < oldest {
			oldest = row.Seq
		}
		if row.Seq > seq {
			seq = row.Seq
		}
		if row.Seq > since {
			events = append(events, json.RawMessage(raw))
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"seq":        seq,
		"oldest_seq": oldest,
		"boot":       g.boot,
		"events":     events,
	})
}

// add appends one new event and returns its seq.
func (g *fakeGateway) add(tag string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	seq := int64(len(g.events) + 1)
	g.events = append(g.events, fmt.Sprintf(
		`{"seq":%d,"boot":%q,"request_id":%q,"model":"m","elapsed_s":1}`,
		seq, g.boot, tag))
	return seq
}

func resetRelayState(t *testing.T, path string, recovered map[string]int64) {
	t.Helper()
	t.Setenv("LATTICE_GATEWAY_TELEMETRY_LOCAL", path)
	relayMu.Lock()
	relayCursors = map[string]relayCursor{}
	relayRecovered = recovered
	relayMu.Unlock()
}

func readRelayLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// The regression this whole change exists for: two local gateways with
// different boot ids, polled alternately. With one global cursor, each poll of
// the other gateway looked like a restart (bootChanged), resynced, and
// re-appended its whole ring — every cycle, forever. The cursor is now per
// gateway id, so each stream advances on its own events only.
func TestRelayPerGatewayCursorNoDuplicateAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry-gateway.jsonl")
	resetRelayState(t, path, nil)

	a := newFakeGateway(t, "bootA")
	b := newFakeGateway(t, "bootB")
	ga := Gateway{ID: "m6-gateway", Endpoint: a.server.URL}
	gb := Gateway{ID: "m1-gateway", Endpoint: b.server.URL}

	// The first poll of each gateway seeds past whatever is buffered and
	// delivers nothing: those events predate this process.
	pullGatewayTelemetry(ga)
	pullGatewayTelemetry(gb)
	if got := readRelayLines(t, path); len(got) != 0 {
		t.Fatalf("seeding appended %d lines, want 0: %v", len(got), got)
	}

	// Five cycles, one new event per gateway per cycle, polled alternately.
	for i := 0; i < 5; i++ {
		a.add(fmt.Sprintf("a-%d", i))
		b.add(fmt.Sprintf("b-%d", i))
		pullGatewayTelemetry(ga)
		pullGatewayTelemetry(gb)
	}

	lines := readRelayLines(t, path)
	if len(lines) != 10 {
		t.Fatalf("appended %d lines, want 10 (one per new event, no re-append): %v", len(lines), lines)
	}
	seen := map[string]int{}
	for _, l := range lines {
		seen[l]++
		if seen[l] > 1 {
			t.Fatalf("duplicate relay line appended: %s", l)
		}
	}

	// Every relayed line is attributed to the gateway that produced it.
	for _, l := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(l), &row); err != nil {
			t.Fatalf("relay line is not JSON: %v (%s)", err, l)
		}
		if row["gateway"] != "m6-gateway" && row["gateway"] != "m1-gateway" {
			t.Fatalf("relay line missing gateway attribution: %s", l)
		}
	}

	// Each cursor advanced to its own stream's head, independently.
	if c := relayCursorFor("m6-gateway"); !c.known || c.seq != 5 || c.boot != "bootA" {
		t.Fatalf("m6 cursor = %+v, want known seq 5 bootA", c)
	}
	if c := relayCursorFor("m1-gateway"); !c.known || c.seq != 5 || c.boot != "bootB" {
		t.Fatalf("m1 cursor = %+v, want known seq 5 bootB", c)
	}
}

// A restart must resume every gateway at its own boot's max seq. The relay file
// holds interleaved streams; recovery is keyed by boot, so resuming one must
// not replay the other.
func TestRelayRecoveryPerGatewayBoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telemetry-gateway.jsonl")

	rows := []string{
		`{"seq":1,"boot":"bootA","gateway":"m6-gateway"}`,
		`{"seq":1,"boot":"bootB","gateway":"m1-gateway"}`,
		`{"seq":2,"boot":"bootA","gateway":"m6-gateway"}`,
		`{"seq":3,"boot":"bootB","gateway":"m1-gateway"}`,
		`{"seq":3,"boot":"bootA","gateway":"m6-gateway"}`,
		`{"seq":2,"boot":"bootB","gateway":"m1-gateway"}`,
		`{"seq":5,"boot":"bootA","gateway":"m6-gateway"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	rec := recoverRelayCursors(path)
	if rec["bootA"] != 5 || rec["bootB"] != 3 {
		t.Fatalf("recovered = %v, want bootA:5 bootB:3", rec)
	}

	resetRelayState(t, path, rec)

	a := newFakeGateway(t, "bootA")
	b := newFakeGateway(t, "bootB")
	for i := 0; i < 5; i++ {
		a.add(fmt.Sprintf("a-%d", i))
	}
	for i := 0; i < 3; i++ {
		b.add(fmt.Sprintf("b-%d", i))
	}
	ga := Gateway{ID: "m6-gateway", Endpoint: a.server.URL}
	gb := Gateway{ID: "m1-gateway", Endpoint: b.server.URL}

	before := len(readRelayLines(t, path))
	pullGatewayTelemetry(ga)
	pullGatewayTelemetry(gb)
	after := readRelayLines(t, path)
	if len(after) != before {
		t.Fatalf("recovery re-appended %d lines, want 0: %v", len(after)-before, after[before:])
	}

	if c := relayCursorFor("m6-gateway"); !c.known || c.seq != 5 || c.boot != "bootA" {
		t.Fatalf("m6 cursor = %+v, want known seq 5 bootA", c)
	}
	if c := relayCursorFor("m1-gateway"); !c.known || c.seq != 3 || c.boot != "bootB" {
		t.Fatalf("m1 cursor = %+v, want known seq 3 bootB", c)
	}

	// A genuinely new event on each stream is delivered exactly once, even if
	// the other stream is polled in between.
	a.add("new-a")
	b.add("new-b")
	pullGatewayTelemetry(ga)
	pullGatewayTelemetry(gb)
	pullGatewayTelemetry(ga)
	pullGatewayTelemetry(gb)

	grown := readRelayLines(t, path)
	if len(grown) != before+2 {
		t.Fatalf("after new events appended %d lines, want 2: %v", len(grown)-before, grown[before:])
	}
	if !strings.Contains(grown[before], "new-a") || !strings.Contains(grown[before+1], "new-b") {
		t.Fatalf("unexpected new lines: %v", grown[before:])
	}
}

// An unfamiliar boot id must seed (deliver nothing) rather than replay the
// ring, and must not adopt another boot's recovered cursor.
func TestRelayUnknownBootSeedsWithoutAppending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry-gateway.jsonl")
	resetRelayState(t, path, map[string]int64{"other-boot": 99})

	g := newFakeGateway(t, "fresh-boot")
	for i := 0; i < 3; i++ {
		g.add(fmt.Sprintf("e-%d", i))
	}
	gw := Gateway{ID: "m6-gateway", Endpoint: g.server.URL}

	pullGatewayTelemetry(gw)
	if lines := readRelayLines(t, path); len(lines) != 0 {
		t.Fatalf("unknown boot appended %d lines, want 0: %v", len(lines), lines)
	}
	c := relayCursorFor("m6-gateway")
	if !c.known || c.seq != 3 || c.boot != "fresh-boot" {
		t.Fatalf("cursor = %+v, want known seq 3 boot fresh-boot", c)
	}

	// A quiet second poll still appends nothing and keeps the seed position.
	pullGatewayTelemetry(gw)
	if lines := readRelayLines(t, path); len(lines) != 0 {
		t.Fatalf("quiet second poll appended %d lines, want 0", len(lines))
	}
	if c := relayCursorFor("m6-gateway"); c.seq != 3 || !c.known {
		t.Fatalf("cursor moved on a quiet poll: %+v", c)
	}
}

// Recovery reads only the tail, so a boot id older than the window is seeded
// rather than resumed. The window is 256 KiB because two gateways interleave.
func TestRecoverRelayCursorsTailWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telemetry-gateway.jsonl")

	const filler = `{"seq":1,"boot":"filler","gateway":"g"}` + "\n"
	body := `{"seq":42,"boot":"ancient","gateway":"g"}` + "\n" +
		strings.Repeat(filler, 7000) +
		`{"seq":7,"boot":"recent","gateway":"g"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	rec := recoverRelayCursors(path)
	if _, ok := rec["ancient"]; ok {
		t.Fatalf("boot outside the tail window was recovered: %v", rec)
	}
	if rec["recent"] != 7 {
		t.Fatalf("recent = %d, want 7 (map %v)", rec["recent"], rec)
	}
}

func TestRecoverRelayCursorsMissingAndEmpty(t *testing.T) {
	if rec := recoverRelayCursors(filepath.Join(t.TempDir(), "absent.jsonl")); len(rec) != 0 {
		t.Fatalf("missing file = %v, want empty", rec)
	}
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if rec := recoverRelayCursors(path); len(rec) != 0 {
		t.Fatalf("empty file = %v, want empty", rec)
	}
}

// A gap marker must name the gateway and boot that lost events, so readers (and
// per-gateway recovery) can attribute it.
func TestGapMarkerCarriesGatewayAndBoot(t *testing.T) {
	raw := gapMarker(7, "m1-gateway", "bootB", "gateway restarted", 0)
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	if row["gateway"] != "m1-gateway" || row["boot"] != "bootB" {
		t.Fatalf("gap marker = %s", raw)
	}
	if row["seq"] != float64(7) || row["error"] == "" {
		t.Fatalf("gap marker lost fields: %s", raw)
	}
}

// tagGateway adds attribution without dropping anything the feeder reads.
func TestTagGatewayPreservesFields(t *testing.T) {
	raw := json.RawMessage(`{"seq":1,"request_id":"r","elapsed_s":2,"error":"boom"}`)
	var row map[string]any
	if err := json.Unmarshal(tagGateway(raw, "m1-gateway"), &row); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{
		"seq": float64(1), "request_id": "r", "elapsed_s": float64(2),
		"error": "boom", "gateway": "m1-gateway",
	} {
		if row[k] != want {
			t.Fatalf("tagged row[%q] = %v, want %v (%v)", k, row[k], want, row)
		}
	}

	// An event that is already attributed is left to its producer.
	already := json.RawMessage(`{"seq":1,"gateway":"origin"}`)
	var row2 map[string]any
	json.Unmarshal(tagGateway(already, "m1-gateway"), &row2)
	if row2["gateway"] != "origin" {
		t.Fatalf("tagGateway overwrote existing attribution: %v", row2)
	}

	// A non-object payload is relayed verbatim rather than dropped.
	if got := string(tagGateway(json.RawMessage(`"just-a-string"`), "m1-gateway")); got != `"just-a-string"` {
		t.Fatalf("non-object payload = %s", got)
	}
}

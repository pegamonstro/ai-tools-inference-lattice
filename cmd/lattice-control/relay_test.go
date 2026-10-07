package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The relay protocol is one-directional: the gateway reports where its event
// stream is, but a consumer cannot tell "nothing new" from "there is a hole"
// unless it compares its cursor against the gateway's identity and against both
// ends of what the gateway still holds. These cases pin the ways it breaks.
func TestPlanRelay(t *testing.T) {
	cases := []struct {
		name        string
		known       bool
		cursor      int64
		producerSeq int64
		oldestSeq   int64
		bootChanged bool
		wantSeed    bool
		wantFetch   bool
		wantSince   int64
		wantNext    int64
		wantGap     bool
		wantLost    int64
	}{
		{
			name: "first pull seeds without replaying",
			// Nothing recovered from the relay file: everything buffered
			// predates this process, so delivering it would duplicate lines
			// already on the Bee screen.
			known: false, cursor: 0, producerSeq: 250, oldestSeq: 1,
			wantSeed: true, wantNext: 250, wantLost: -1,
		},
		{
			name: "a known cursor of zero is NOT a fresh start",
			// Regression: a resync can legitimately land on zero. Treating that
			// as "unknown" makes every later poll re-seed and swallow the next
			// event -- which is how a request went missing during development.
			known: true, cursor: 0, producerSeq: 1, oldestSeq: 1,
			wantNext: 1, wantLost: -1,
		},
		{
			name:  "steady state delivers what is new",
			known: true, cursor: 5, producerSeq: 9, oldestSeq: 1,
			wantNext: 9, wantLost: -1,
		},
		{
			name:  "no new events is not a gap",
			known: true, cursor: 9, producerSeq: 9, oldestSeq: 1,
			wantNext: 9, wantLost: -1,
		},
		{
			name: "restart with a counter behind the cursor costs nothing",
			// The gateway began again at 1, but everything it has produced so
			// far is still in the ring, so a resync delivers all of it. Lost is
			// a known 0, not the unknown -1: we can account for every event.
			known: true, cursor: 17, producerSeq: 4, oldestSeq: 1,
			wantFetch: true, wantSince: 0, wantNext: 4, wantLost: 0,
		},
		{
			name: "new boot id whose counter has NOT fallen behind the cursor",
			// The case the seq comparison cannot see: the new incarnation's
			// "seq 1" looks exactly like the one already delivered. Without the
			// boot id this event is skipped silently; with it, the resync
			// delivers it and nothing is lost.
			known: true, cursor: 1, producerSeq: 1, oldestSeq: 1, bootChanged: true,
			wantFetch: true, wantSince: 0, wantNext: 1, wantLost: 0,
		},
		{
			name: "new boot id whose ring already evicted early events",
			// 300 events into the new run, the ring holds 100..300 -- so 1..99
			// are gone, and after a restart the count runs from 1.
			known: true, cursor: 200, producerSeq: 300, oldestSeq: 100, bootChanged: true,
			wantFetch: true, wantSince: 99, wantNext: 300,
			wantGap: true, wantLost: 99,
		},
		{
			name: "ring eviction drops events never relayed",
			// Nothing restarted; the gateway simply logged more than the 256
			// events the ring holds between two polls.
			known: true, cursor: 5, producerSeq: 300, oldestSeq: 100,
			wantFetch: true, wantSince: 99, wantNext: 300,
			wantGap: true, wantLost: 94,
		},
		{
			name:  "eviction far past a stale cursor",
			known: true, cursor: 17, producerSeq: 400, oldestSeq: 145,
			wantFetch: true, wantSince: 144, wantNext: 400,
			wantGap: true, wantLost: 127,
		},
		{
			name: "restart polled before the new run logs anything costs nothing",
			// The counter is back at zero with an empty ring, so nothing was
			// produced and nothing is gone. Reporting this would put a red line
			// on screen for every routine restart.
			known: true, cursor: 17, producerSeq: 0, oldestSeq: 0, bootChanged: true,
			wantNext: 0, wantLost: 0,
		},
		{
			name: "a gateway too old to report oldest_seq leaves the loss unknown",
			// Without oldest_seq nothing can be said about what it discarded, so
			// the gap is reported as an unknown count rather than a false zero.
			// Reachable during a mixed-version deploy, where the halves ship
			// separately.
			known: true, cursor: 17, producerSeq: 4, oldestSeq: 0,
			wantFetch: false, wantNext: 4, wantGap: true, wantLost: -1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planRelay(tc.known, tc.cursor, tc.producerSeq, tc.oldestSeq, tc.bootChanged)

			if got.Seed != tc.wantSeed {
				t.Errorf("Seed = %v, want %v", got.Seed, tc.wantSeed)
			}
			if got.Fetch != tc.wantFetch {
				t.Errorf("Fetch = %v, want %v", got.Fetch, tc.wantFetch)
			}
			if tc.wantFetch && got.Since != tc.wantSince {
				t.Errorf("Since = %d, want %d", got.Since, tc.wantSince)
			}
			if got.Next != tc.wantNext {
				t.Errorf("Next = %d, want %d", got.Next, tc.wantNext)
			}
			if (got.GapNote != "") != tc.wantGap {
				t.Errorf("GapNote = %q, want gap = %v", got.GapNote, tc.wantGap)
			}
			if got.Lost != tc.wantLost {
				t.Errorf("Lost = %d, want %d", got.Lost, tc.wantLost)
			}
		})
	}
}

// The relay file is the only durable record of what has been delivered, so the
// position a restart resumes from is read back out of it — per gateway, since
// each gateway's cursor advances independently.
func TestRecoverRelayCursors(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "telemetry-gateway.jsonl")
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("missing file is a fresh start", func(t *testing.T) {
		cursors := recoverRelayCursors(filepath.Join(t.TempDir(), "absent.jsonl"))
		if len(cursors) != 0 {
			t.Errorf("got %d cursors, want none", len(cursors))
		}
	})

	t.Run("last line per gateway wins", func(t *testing.T) {
		// Interleaved: the two gateways' rows alternate, and each gateway's
		// cursor must come from its own last row, not the file's last row.
		cursors := recoverRelayCursors(write(t,
			`{"seq":1,"boot":"a1","gateway":"m6"}`+"\n"+
				`{"seq":1,"boot":"b1","gateway":"m1"}`+"\n"+
				`{"seq":2,"boot":"a1","gateway":"m6"}`+"\n"+
				`{"seq":7,"boot":"b1","gateway":"m1"}`+"\n"))
		if len(cursors) != 2 {
			t.Fatalf("got %d cursors, want 2: %v", len(cursors), cursors)
		}
		if got := cursors["m6"]; got.seq != 2 || got.boot != "a1" || !got.known {
			t.Errorf("m6 = %+v, want seq 2 boot a1 known", got)
		}
		if got := cursors["m1"]; got.seq != 7 || got.boot != "b1" || !got.known {
			t.Errorf("m1 = %+v, want seq 7 boot b1 known", got)
		}
	})

	t.Run("rows without a gateway are skipped", func(t *testing.T) {
		// Rows predating stamping cannot be attributed, and a gap marker's
		// model="relay" row belongs to whichever gateway wrote it — an
		// unattributed one must not become some other gateway's cursor.
		cursors := recoverRelayCursors(write(t,
			`{"seq":9,"boot":"old"}`+"\n"+
				`{"seq":2,"boot":"a1","gateway":"m6"}`+"\n"+
				`{"seq":0,"error":"gap"}`+"\n"))
		if len(cursors) != 1 {
			t.Fatalf("got %d cursors, want 1: %v", len(cursors), cursors)
		}
		if cursors["m6"].seq != 2 {
			t.Errorf("m6 = %+v, want seq 2", cursors["m6"])
		}
	})

	t.Run("a torn final line falls back to the last whole one", func(t *testing.T) {
		cursors := recoverRelayCursors(write(t, `{"seq":4,"boot":"a1","gateway":"m6"}`+"\n"+`{"seq":5,"bo`))
		if len(cursors) != 1 || cursors["m6"].seq != 4 || cursors["m6"].boot != "a1" {
			t.Errorf("got %v, want m6 seq 4 boot a1", cursors)
		}
	})

	t.Run("gap marker with its gateway participates", func(t *testing.T) {
		cursors := recoverRelayCursors(write(t,
			`{"seq":2,"boot":"a1","gateway":"m6"}`+"\n"+
				`{"seq":627,"gateway":"m6","model":"relay","request_id":"telemetry-gap","elapsed_s":0,"error":"x"}`+"\n"))
		if len(cursors) != 1 || cursors["m6"].seq != 627 {
			t.Errorf("got %v, want m6 seq 627", cursors)
		}
	})
}

// Two gateways each restart their counters — so any state shared between them
// has every alternating poll flip the boot id and re-relay both rings in full.
// This is the regression for the replay flood: alternating polls must append
// each event exactly once.
func TestTwoGatewaysRelayIndependently(t *testing.T) {
	// fakeRing is a live gateway's buffer: a counter that advances as new
	// inference happens, and a ring whose low end may be evicted. A poll
	// delivers only what the caller has not seen (seq > since) and still
	// reports the counter even when nothing is new.
	type fakeRing struct {
		boot    string
		issued  int64 // highest seq issued — advances as traffic happens
		oldest  int64 // lowest seq the ring still holds
		batches int   // new events issued per poll
	}
	advance := func(r *fakeRing) {
		r.issued += int64(r.batches)
	}
	restart := func(r *fakeRing, boot string, issued, oldest int64) {
		r.boot, r.issued, r.oldest = boot, issued, oldest
	}
	fakeGateway := func(t *testing.T, r *fakeRing) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			var since int64
			fmt.Sscanf(req.URL.Query().Get("since"), "%d", &since)
			var events []json.RawMessage
			from := r.oldest
			if since+1 > from {
				from = since + 1
			}
			for s := from; s <= r.issued; s++ {
				ev := fmt.Sprintf(`{"seq":%d,"boot":%q,"request_id":"%s-%d","model":"m","elapsed_s":0.1}`, s, r.boot, r.boot, s)
				events = append(events, json.RawMessage(ev))
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"seq":        r.issued,
				"oldest_seq": r.oldest,
				"boot":       r.boot,
				"events":     events,
			})
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("alternating polls append each event once", func(t *testing.T) {
		resetRelayCursors(t)
		path := filepath.Join(t.TempDir(), "telemetry-gateway.jsonl")
		t.Setenv("LATTICE_GATEWAY_TELEMETRY_LOCAL", path)
		ra := fakeRing{boot: "boot-a", batches: 2}
		rb := fakeRing{boot: "boot-b", batches: 2}
		aURL := fakeGateway(t, &ra)
		bURL := fakeGateway(t, &rb)

		for round := 0; round < 4; round++ {
			// Traffic happens between polls on both gateways, interleaved.
			advance(&ra)
			advance(&rb)
			pullGatewayTelemetry("gw-a", Gateway{ID: "gw-a", Endpoint: aURL.URL})
			pullGatewayTelemetry("gw-b", Gateway{ID: "gw-b", Endpoint: bURL.URL})
		}

		seen := map[string]bool{}
		dups := 0
		lines := readLines(t, path)
		for _, line := range lines {
			var row struct {
				Seq     int64  `json:"seq"`
				Gateway string `json:"gateway"`
			}
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatalf("bad relay line %q: %v", line, err)
			}
			key := fmt.Sprintf("%s/%d", row.Gateway, row.Seq)
			if seen[key] {
				dups++
			}
			seen[key] = true
		}
		if dups != 0 {
			t.Errorf("relay file has %d duplicated events across %d lines", dups, len(lines))
		}
		// Round 1 seeds (buffered events predate the consumer); rounds 2-4
		// each deliver the 2 new events per gateway the advance calls issued.
		if len(seen) != 12 {
			t.Errorf("relayed %d distinct events, want 12", len(seen))
		}
	})

	t.Run("one gateway restarting does not replay the other", func(t *testing.T) {
		resetRelayCursors(t)
		path := filepath.Join(t.TempDir(), "telemetry-gateway.jsonl")
		t.Setenv("LATTICE_GATEWAY_TELEMETRY_LOCAL", path)
		ra := fakeRing{boot: "boot-a", batches: 1}
		rb := fakeRing{boot: "boot-b", batches: 1}
		aURL := fakeGateway(t, &ra)
		bURL := fakeGateway(t, &rb)

		advance(&ra)
		pullGatewayTelemetry("gw-a", Gateway{ID: "gw-a", Endpoint: aURL.URL})
		advance(&rb)
		pullGatewayTelemetry("gw-b", Gateway{ID: "gw-b", Endpoint: bURL.URL})
		advance(&ra)
		pullGatewayTelemetry("gw-a", Gateway{ID: "gw-a", Endpoint: aURL.URL})
		pullGatewayTelemetry("gw-b", Gateway{ID: "gw-b", Endpoint: bURL.URL}) // quiet for gw-b

		// gw-a restarts: counter now at 5, ring holds only 4 and 5 — the
		// first three events of the new run were evicted before anyone read
		// them. gw-b keeps polling as usual in between.
		restart(&ra, "boot-a2", 5, 4)
		pullGatewayTelemetry("gw-b", Gateway{ID: "gw-b", Endpoint: bURL.URL})
		advance(&rb)
		pullGatewayTelemetry("gw-b", Gateway{ID: "gw-b", Endpoint: bURL.URL})
		pullGatewayTelemetry("gw-a", Gateway{ID: "gw-a", Endpoint: aURL.URL})
		pullGatewayTelemetry("gw-a", Gateway{ID: "gw-a", Endpoint: aURL.URL}) // quiet

		lines := readLines(t, path)
		var gaps, events int
		for _, line := range lines {
			var row struct {
				RequestID string `json:"request_id"`
			}
			json.Unmarshal([]byte(line), &row)
			if row.RequestID == "telemetry-gap" {
				gaps++
			} else {
				events++
			}
		}
		// gw-a: 1 before its restart + 2 by the restart resync (each
		// gateway's first event was buffered before its first poll, so its
		// own seed skipped it); gw-b: 1. Quiet and interleaved polls
		// delivered none.
		if events != 4 {
			t.Errorf("relayed %d events, want 4", events)
		}
		if gaps != 1 {
			t.Errorf("got %d gap markers, want 1 (one restart, one gateway)", gaps)
		}
	})
}

// The cursor map is package state that survives across subtests in one test
// binary; production resets it at startup via recovery, tests reset it here.
func resetRelayCursors(t *testing.T) {
	t.Helper()
	gatewayRelayMu.Lock()
	gatewayRelayCursors = map[string]gatewayRelay{}
	gatewayRelayMu.Unlock()
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

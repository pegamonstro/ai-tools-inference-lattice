package main

import (
	"os"
	"path/filepath"
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
			name:  "a known cursor of zero is NOT a fresh start",
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
// cursor and boot id a restart resumes from are read back out of it.
func TestRecoverRelayState(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "telemetry-gateway.jsonl")
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("missing file is a fresh start", func(t *testing.T) {
		seq, boot := recoverRelayState(filepath.Join(t.TempDir(), "absent.jsonl"))
		if seq != 0 || boot != "" {
			t.Errorf("got (%d, %q), want (0, \"\")", seq, boot)
		}
	})

	t.Run("empty file is a fresh start", func(t *testing.T) {
		seq, boot := recoverRelayState(write(t, ""))
		if seq != 0 || boot != "" {
			t.Errorf("got (%d, %q), want (0, \"\")", seq, boot)
		}
	})

	t.Run("last line wins", func(t *testing.T) {
		seq, boot := recoverRelayState(write(t, `{"seq":1,"boot":"aaa","elapsed_s":1}`+"\n"+
			`{"seq":2,"boot":"aaa","elapsed_s":1}`+"\n"))
		if seq != 2 || boot != "aaa" {
			t.Errorf("got (%d, %q), want (2, \"aaa\")", seq, boot)
		}
	})

	t.Run("trailing newline and blank lines are ignored", func(t *testing.T) {
		seq, _ := recoverRelayState(write(t, `{"seq":7,"boot":"bbb"}`+"\n\n"))
		if seq != 7 {
			t.Errorf("seq = %d, want 7", seq)
		}
	})

	t.Run("a torn final line falls back to the last whole one", func(t *testing.T) {
		seq, boot := recoverRelayState(write(t, `{"seq":4,"boot":"ccc"}`+"\n"+`{"seq":5,"bo`))
		if seq != 4 || boot != "ccc" {
			t.Errorf("got (%d, %q), want (4, \"ccc\")", seq, boot)
		}
	})

	t.Run("gap marker with seq zero does not become the cursor", func(t *testing.T) {
		// A marker written when the gateway had logged nothing carries seq 0;
		// it must not be mistaken for a resumed position.
		seq, _ := recoverRelayState(write(t, `{"seq":9,"boot":"ddd"}`+"\n"+`{"seq":0,"error":"gap"}`+"\n"))
		if seq != 9 {
			t.Errorf("seq = %d, want 9", seq)
		}
	})
}

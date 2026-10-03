package main

import (
	"context"
	"testing"
	"time"
)

func TestComputeSlots(t *testing.T) {
	// Sizes mirror the M6's resident catalog (bytes): gemma4:31b 20GB, 26b 18GB,
	// 12b 8GB, qwen3-vl:8b 6.1GB, bge-m3 1.2GB.
	gb := func(n float64) uint64 { return uint64(n * 1024 * 1024 * 1024) }
	sizes := []int64{int64(gb(20)), int64(gb(18)), int64(gb(8)), int64(gb(6.1)), int64(gb(1.2))}

	cases := []struct {
		name      string
		available uint64
		max       int
		want      int
	}{
		{"idle headroom fits big+2small", gb(24), 3, 3},
		{"one big resident leaves 2 small", gb(7.6), 3, 2},
		{"no memory floors to one", 0, 3, 1},
		{"max cap holds despite many tiny", gb(24), 2, 2},
		{"max below one clamps to one", gb(24), 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := computeSlots(sizes, tc.available, tc.max); got != tc.want {
				t.Fatalf("computeSlots(available=%d, max=%d) = %d, want %d",
					tc.available, tc.max, got, tc.want)
			}
		})
	}

	// Empty/zero sizes still yield at least one slot.
	if got := computeSlots(nil, gb(24), 3); got != 1 {
		t.Fatalf("computeSlots(nil) = %d, want 1", got)
	}
}

func TestSlotSemaphoreBasic(t *testing.T) {
	s := newSlotSemaphore(2)
	if !s.acquire(context.Background()) {
		t.Fatal("first acquire failed")
	}
	if !s.acquire(context.Background()) {
		t.Fatal("second acquire failed")
	}

	// A third acquire must block while both slots are held.
	done := make(chan bool, 1)
	go func() { done <- s.acquire(context.Background()) }()
	select {
	case <-done:
		t.Fatal("third acquire should have blocked")
	case <-time.After(30 * time.Millisecond):
	}

	s.release()
	select {
	case got := <-done:
		if !got {
			t.Fatal("acquire after release failed")
		}
	case <-time.After(time.Second):
		t.Fatal("acquire did not proceed after release")
	}
}

func TestSlotSemaphoreResize(t *testing.T) {
	s := newSlotSemaphore(1)
	if !s.acquire(context.Background()) {
		t.Fatal("first acquire failed")
	}

	done := make(chan bool, 1)
	go func() { done <- s.acquire(context.Background()) }()
	select {
	case <-done:
		t.Fatal("second acquire should have blocked at limit 1")
	case <-time.After(30 * time.Millisecond):
	}

	// Raising the limit frees the queued acquire.
	s.setLimit(2)
	select {
	case got := <-done:
		if !got {
			t.Fatal("acquire failed after limit raise")
		}
	case <-time.After(time.Second):
		t.Fatal("acquire did not proceed after limit raise")
	}

	if got := s.limitValue(); got != 2 {
		t.Fatalf("limitValue = %d, want 2", got)
	}
}

func TestSlotSemaphoreCancel(t *testing.T) {
	s := newSlotSemaphore(1)
	if !s.acquire(context.Background()) {
		t.Fatal("first acquire failed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- s.acquire(ctx) }()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case got := <-done:
		if got {
			t.Fatal("acquire returned true after cancel, want false")
		}
	case <-time.After(time.Second):
		t.Fatal("acquire did not return after cancel")
	}
}

package main

import "testing"

// macOS "free" understates the memory that can come back without disk IO:
// purgeable pages are marked droppable by their owner and speculative pages
// are already prefetched and unclaimed. The budgeter must count them in, or
// every load is judged against a smaller pool than exists and heavy jobs get
// rejected while the box is fine.
func TestVMStatReclaimableCountsPurgeableAndSpeculative(t *testing.T) {
	stat := []byte(`Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free: 24126.
Pages active: 1944840.
Pages inactive: 1084275.
Pages speculative: 2861.
Pages throttled: 0.
Pages purgeable: 97512.
Pages occupied by compressor: 191243.
Pages wired down: 404014.
Pages copy-on-write: 4321.
Pages zero filled: 123456.
Pages reactivated: 98765.
Pageins: 111.
Pageouts: 22.
Swapins: 33.
Swapouts: 44.`)

	want := uint64(24126+1084275+97512+2861) * 16384
	if got := vmStatReclaimable(stat, 16384); got != want {
		t.Fatalf("reclaimable = %d, want %d (free + inactive + purgeable + speculative; compressor and wired excluded)", got, want)
	}
}

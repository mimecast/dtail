package pool

import (
	"bytes"
	"testing"
)

// TestRecycleBytesBufferDropsLargeBuffers checks that a buffer grown above
// MaxRecycledBytesBufferCap is not put back into the pool, while a buffer of
// at most that capacity is reset for the pool. (Whether the pool then hands a
// buffer out again is up to sync.Pool, which drops some under the race
// detector, so only the dropped buffers are checked against a Get.)
func TestRecycleBytesBufferDropsLargeBuffers(t *testing.T) {
	tests := []struct {
		name     string
		capacity int
		pooled   bool
	}{
		{name: "default capacity", capacity: 4096, pooled: true},
		{name: "exactly the maximum", capacity: MaxRecycledBytesBufferCap, pooled: true},
		{name: "above the maximum", capacity: MaxRecycledBytesBufferCap + 1, pooled: false},
		{name: "several MiB", capacity: 8 * MaxRecycledBytesBufferCap, pooled: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := bytes.NewBuffer(make([]byte, 0, tt.capacity))
			buf.WriteString("line")
			RecycleBytesBuffer(buf)
			if !tt.pooled {
				if got := BytesBuffer.Get().(*bytes.Buffer); got == buf {
					t.Fatalf("buffer of capacity %d was pooled", tt.capacity)
				}
				return
			}
			if buf.Len() != 0 {
				t.Fatalf("recycled buffer holds %d bytes, want it reset", buf.Len())
			}
		})
	}
}

// TestRecycleBytesBufferKeepsDefaultMaxLineBuffers checks buffers grown the way
// the read paths grow them: one that held a line of the default MaxLineLength
// (1 MiB) plus its newline, rounded up by bytes.Buffer, and one that doubled
// from 600 KiB, are both pooled again; one that held a line of a larger
// MaxLineLength (4 MiB plus its newline) ends above the threshold and is
// dropped.
func TestRecycleBytesBufferKeepsDefaultMaxLineBuffers(t *testing.T) {
	const mib = 1024 * 1024
	tests := []struct {
		name    string
		initial int
		writes  []int
		minCap  int
		pooled  bool
	}{
		{name: "default maximum line plus newline", initial: 4096,
			writes: []int{mib + 1}, minCap: mib + 1, pooled: true},
		{name: "doubled from 600 KiB", initial: 600 * 1024,
			writes: []int{600 * 1024, 1}, minCap: 2 * 600 * 1024, pooled: true},
		{name: "4 MiB line plus newline", initial: 4096,
			writes: []int{4*mib + 1}, minCap: 4*mib + 1, pooled: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := bytes.NewBuffer(make([]byte, 0, tt.initial))
			for _, n := range tt.writes {
				buf.Write(bytes.Repeat([]byte{'x'}, n))
			}
			if buf.Cap() < tt.minCap {
				t.Fatalf("buffer capacity %d, want at least %d", buf.Cap(), tt.minCap)
			}
			if got := buf.Cap() <= MaxRecycledBytesBufferCap; got != tt.pooled {
				t.Fatalf("capacity %d within MaxRecycledBytesBufferCap %d = %v, want %v",
					buf.Cap(), MaxRecycledBytesBufferCap, got, tt.pooled)
			}
			RecycleBytesBuffer(buf)
			// RecycleBytesBuffer resets exactly the buffers it pools.
			if reset := buf.Len() == 0; reset != tt.pooled {
				t.Fatalf("buffer of capacity %d reset (pooled) = %v, want %v",
					buf.Cap(), reset, tt.pooled)
			}
		})
	}
}

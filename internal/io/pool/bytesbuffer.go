package pool

import (
	"bytes"
	"sync"
)

// BytesBuffer is there to optimize memory allocations. DTail otherwise allocates
// a lot of memory while reading logs.
var BytesBuffer = sync.Pool{
	New: func() any {
		b := bytes.Buffer{}
		// Increase initial capacity to 4KB to reduce reallocations
		// Most log lines are between 100-500 bytes, but some can be larger
		b.Grow(4096)
		return &b
	},
}

// MaxRecycledBytesBufferCap is the largest capacity of a buffer that
// RecycleBytesBuffer puts back into BytesBuffer. It sits well above a line of
// the default MaxLineLength (1 MiB) plus its newline: bytes.Buffer rounds such
// a buffer up past the line (1 MiB + 1 byte becomes 1,056,768 bytes of
// capacity), and a buffer grown by doubling may end at up to about twice the
// line (600 KiB doubled is 1,228,800 bytes). A limit of exactly 1 MiB dropped
// all of these, so lines of 512 KiB to 1 MiB produced garbage on every read
// instead of reusing their buffers. Buffers above 4 MiB only arise from
// lines a MaxLineLength above the default allows (it may allow several MiB);
// pooling those would keep that memory pinned for later lines that are mostly
// a few hundred bytes long, so they are left to the garbage collector instead.
const MaxRecycledBytesBufferCap = 4 * 1024 * 1024

// RecycleBytesBuffer resets the buffer and puts it back into BytesBuffer,
// unless its capacity is above MaxRecycledBytesBufferCap.
func RecycleBytesBuffer(b *bytes.Buffer) {
	if b.Cap() > MaxRecycledBytesBufferCap {
		return
	}
	b.Reset()
	BytesBuffer.Put(b)
}

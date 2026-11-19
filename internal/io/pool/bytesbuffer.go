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

// RecycleBytesBuffer recycles the buffer again.
func RecycleBytesBuffer(b *bytes.Buffer) {
	b.Reset()
	BytesBuffer.Put(b)
}

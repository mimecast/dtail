package pool

import (
	"bytes"
	"sync"
)

// BytesBuffer is there to optimize memory allocations. DTail otherwise allocates
// a lot of memory while reading logs.
var BytesBuffer = sync.Pool{
	New: func() interface{} {
		b := bytes.Buffer{}
		b.Grow(128)
		return &b
	},
}

// RecycleBytesBuffer recycles the buffer again.
func RecycleBytesBuffer(b *bytes.Buffer) {
	b.Reset()
	BytesBuffer.Put(b)
}

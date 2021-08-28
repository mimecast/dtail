package pool

import (
	"bytes"
	"sync"
)

var BytesBuffer = sync.Pool{
	New: func() interface{} {
		b := bytes.Buffer{}
		b.Grow(128)
		return &b
	},
}

func RecycleBytesBuffer(b *bytes.Buffer) {
	b.Reset()
	BytesBuffer.Put(b)
}

package pool

import (
	"sync"
)

// ScannerBufferSize is the longest line, without its newline, that a buffer of
// ScannerBufferPool holds whole: 1 MiB, the default MaxLineLength. The buffers
// hold ScannerBufferCapacity bytes, one more, for the newline after such a
// line.
const ScannerBufferSize = 1024 * 1024

// ScannerBufferCapacity is the length and capacity of the buffers
// ScannerBufferPool hands out. bufio.Scanner uses the capacity of the buffer
// it is given, so a snapshot read with the default MaxLineLength, whose
// maximum token size is MaxLineLength + 1, never grows out of the pooled
// buffer, not even for a line longer than 1 MiB.
const ScannerBufferCapacity = ScannerBufferSize + 1

// ScannerBufferPool provides a pool of ScannerBufferCapacity (1 MiB + 1 byte)
// buffers for scanner operations to reduce allocation overhead in the
// direct-output read path. A scanner uses the pooled buffer as its initial
// buffer; when its maximum token size is larger (a snapshot read with a
// MaxLineLength above 1 MiB), bufio.Scanner grows into a buffer of its own for
// a longer line, and the pooled one is returned unchanged.
var ScannerBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, ScannerBufferCapacity)
		return &buf
	},
}

// MediumBufferPool provides a pool of 64KB buffers for tail mode reads
var MediumBufferPool = sync.Pool{
	New: func() any {
		// 64KB buffer for medium-sized operations
		buf := make([]byte, 64*1024)
		return &buf
	},
}

// SmallBufferPool provides a pool of 4KB buffers for small operations
var SmallBufferPool = sync.Pool{
	New: func() any {
		// 4KB buffer for small operations
		buf := make([]byte, 4*1024)
		return &buf
	},
}

// GetScannerBuffer gets a ScannerBufferCapacity buffer from the pool
func GetScannerBuffer() *[]byte {
	return ScannerBufferPool.Get().(*[]byte)
}

// PutScannerBuffer returns a scanner buffer to the pool
func PutScannerBuffer(buf *[]byte) {
	// Restore the full length before returning the reusable backing array.
	if buf != nil && len(*buf) > 0 {
		*buf = (*buf)[:cap(*buf)]
	}
	ScannerBufferPool.Put(buf)
}

// GetMediumBuffer gets a 64KB buffer from the pool
func GetMediumBuffer() *[]byte {
	return MediumBufferPool.Get().(*[]byte)
}

// PutMediumBuffer returns a medium buffer to the pool
func PutMediumBuffer(buf *[]byte) {
	// Restore the full length before returning the reusable backing array.
	if buf != nil && len(*buf) > 0 {
		*buf = (*buf)[:cap(*buf)]
	}
	MediumBufferPool.Put(buf)
}

// GetSmallBuffer gets a 4KB buffer from the pool. Its contents are unspecified;
// callers must overwrite the bytes they use before reading them, as bufio.Scanner
// does with buffers supplied through Scanner.Buffer.
func GetSmallBuffer() *[]byte {
	return SmallBufferPool.Get().(*[]byte)
}

// PutSmallBuffer returns a small buffer to the pool with its full capacity
// available. It deliberately leaves the contents unchanged because callers must
// overwrite pooled storage before reading it.
func PutSmallBuffer(buf *[]byte) {
	if !prepareSmallBuffer(buf) {
		return
	}
	SmallBufferPool.Put(buf)
}

func prepareSmallBuffer(buf *[]byte) bool {
	if buf == nil {
		return false
	}
	*buf = (*buf)[:cap(*buf)]
	return true
}

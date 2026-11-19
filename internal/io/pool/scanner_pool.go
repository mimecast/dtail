package pool

import (
	"sync"
)

// ScannerBufferPool provides a pool of 1MB buffers for scanner operations
// to reduce allocation overhead in the direct-output read path
var ScannerBufferPool = sync.Pool{
	New: func() any {
		// 1MB buffer for scanner operations
		buf := make([]byte, 1024*1024)
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

// GetScannerBuffer gets a 1MB buffer from the pool
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

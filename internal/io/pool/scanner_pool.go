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

// GetSmallBuffer gets a 4KB buffer from the pool
func GetSmallBuffer() *[]byte {
	return SmallBufferPool.Get().(*[]byte)
}

// PutSmallBuffer returns a small buffer to the pool
func PutSmallBuffer(buf *[]byte) {
	// Clear the buffer before returning to pool
	if buf != nil && len(*buf) > 0 {
		*buf = (*buf)[:cap(*buf)]
		for i := range *buf {
			(*buf)[i] = 0
		}
	}
	SmallBufferPool.Put(buf)
}

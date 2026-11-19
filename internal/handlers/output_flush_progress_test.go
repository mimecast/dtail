package handlers

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// A reader that keeps draining, only slower than one flush timeout for the
// whole buffer, must not fail the flush: the timeout bounds a stalled
// reader, not the whole drain.
func TestOutputManagerFlushWaitsForSlowButProgressingReader(t *testing.T) {
	const flushTimeout = 40 * time.Millisecond
	manager := &outputManager{}
	manager.configure(outputManagerConfig{flushTimeout: flushTimeout}, handlerTestLogger)
	manager.enable()
	mustEnqueueOutput(t, manager, 0, bytes.Repeat([]byte("x"), 64))

	// Read 4 bytes every quarter timeout: draining takes about four timeouts.
	go func() {
		buf := make([]byte, 4)
		for manager.bufferedLen() > 0 {
			manager.tryRead(buf, nil, func(uint64) bool { return false })
			time.Sleep(flushTimeout / 4)
		}
	}()

	if err := manager.flush(context.Background(), nil); err != nil {
		t.Fatalf("flush with a slowly progressing reader failed: %v", err)
	}
}

// While the session reader is still delivering to a paused client, the flush
// goes on waiting instead of timing out; it still times out once that is no
// longer the case.
func TestOutputManagerFlushWaitsWhileClientIsSlow(t *testing.T) {
	const flushTimeout = 10 * time.Millisecond
	manager := &outputManager{}
	manager.configure(outputManagerConfig{flushTimeout: flushTimeout}, handlerTestLogger)
	var slow atomic.Bool
	slow.Store(true)
	manager.clientSlow = slow.Load
	manager.enable()
	mustEnqueueOutput(t, manager, 0, []byte("paused"))

	flushDone := make(chan error, 1)
	go func() { flushDone <- manager.flush(context.Background(), nil) }()
	select {
	case err := <-flushDone:
		t.Fatalf("flush returned while the client was slow: %v", err)
	case <-time.After(10 * flushTimeout):
	}

	slow.Store(false)
	select {
	case err := <-flushDone:
		if !errors.Is(err, errOutputFlushTimeout) {
			t.Fatalf("flush error = %v, want %v", err, errOutputFlushTimeout)
		}
	case <-time.After(time.Second):
		t.Fatal("flush did not time out once the client was no longer slow")
	}
}

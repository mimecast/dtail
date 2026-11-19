package handlers

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/protocol"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

func TestOutputManagerBackpressuresAtByteLimit(t *testing.T) {
	manager := &outputManager{}
	manager.configure(outputManagerConfig{
		bufferMaxBytes:    8,
		readRetryInterval: time.Millisecond,
	}, handlerTestLogger)
	manager.enable()
	mustEnqueueOutput(t, manager, 1, []byte("abcdefgh"))

	enqueueDone := make(chan error, 1)
	go func() {
		enqueueDone <- manager.enqueue(context.Background(), 1, []byte("i"), nil)
	}()

	select {
	case err := <-enqueueDone:
		t.Fatalf("enqueue returned before buffer space was available: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	buf := make([]byte, 4)
	n, handled := manager.tryRead(buf, &userserver.User{Name: "byte-limit-test"}, nil)
	if !handled || string(buf[:n]) != "abcd" {
		t.Fatalf("first read = handled %v, payload %q; want abcd", handled, buf[:n])
	}

	select {
	case err := <-enqueueDone:
		t.Fatalf("enqueue returned while a partial slice retained the full backing allocation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	manager.mu.Lock()
	if got := manager.bufferedBytes; got != 4 {
		manager.mu.Unlock()
		t.Fatalf("undelivered bytes during partial drain = %d, want 4", got)
	}
	if got := manager.retainedBytes; got != 8 {
		manager.mu.Unlock()
		t.Fatalf("retained backing bytes during partial drain = %d, want 8", got)
	}
	if got := manager.buffer.retainedBytes; got != 8 {
		manager.mu.Unlock()
		t.Fatalf("partial buffer allocation credit = %d, want 8", got)
	}
	manager.mu.Unlock()

	n, handled = manager.tryRead(buf, &userserver.User{Name: "byte-limit-test"}, nil)
	if !handled || string(buf[:n]) != "efgh" {
		t.Fatalf("second read = handled %v, payload %q; want efgh", handled, buf[:n])
	}
	select {
	case err := <-enqueueDone:
		if err != nil {
			t.Fatalf("enqueue after backing release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("enqueue remained blocked after backing allocation was released")
	}
	if got := manager.bufferedLen(); got != 1 {
		t.Fatalf("buffered bytes after backing release = %d, want 1", got)
	}
	n, handled = manager.tryRead(buf, &userserver.User{Name: "byte-limit-test"}, nil)
	if !handled || string(buf[:n]) != "i" {
		t.Fatalf("third read = handled %v, payload %q; want i", handled, buf[:n])
	}
}

func TestOutputManagerBlockedEnqueueHonorsContext(t *testing.T) {
	manager := &outputManager{}
	manager.configure(outputManagerConfig{
		bufferMaxBytes:    4,
		readRetryInterval: time.Millisecond,
	}, handlerTestLogger)
	manager.enable()
	mustEnqueueOutput(t, manager, 0, []byte("full"))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := manager.enqueue(ctx, 0, []byte("x"), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("enqueue error = %v, want context deadline exceeded", err)
	}
	if got := manager.bufferedLen(); got != 4 {
		t.Fatalf("buffered bytes after canceled enqueue = %d, want 4", got)
	}
}

func TestOutputManagerBlockedEnqueueDropsStaleGeneration(t *testing.T) {
	manager := &outputManager{}
	manager.configure(outputManagerConfig{
		bufferMaxBytes:    4,
		readRetryInterval: time.Millisecond,
	}, handlerTestLogger)
	manager.enable()
	mustEnqueueOutput(t, manager, 1, []byte("full"))

	var activeGeneration atomic.Uint64
	activeGeneration.Store(1)
	done := make(chan error, 1)
	go func() {
		done <- manager.enqueue(context.Background(), 1, []byte("x"), activeGeneration.Load)
	}()
	activeGeneration.Store(2)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stale enqueue returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stale enqueue did not stop after generation changed")
	}
	if got := manager.bufferedLen(); got != 4 {
		t.Fatalf("buffered bytes after stale enqueue = %d, want 4", got)
	}
}

func TestOutputManagerOversizedPayloadIsRejectedWithoutPrefix(t *testing.T) {
	manager := &outputManager{}
	manager.configure(outputManagerConfig{
		bufferMaxBytes:    4,
		readRetryInterval: time.Millisecond,
	}, handlerTestLogger)
	manager.enable()

	ctx, cancel := context.WithCancel(context.Background())
	err := manager.enqueue(ctx, 1, []byte("abcdefgh"), nil)
	if !errors.Is(err, errOutputPayloadTooLarge) {
		t.Fatalf("enqueue error = %v, want payload-too-large error", err)
	}
	cancel()

	if got := manager.bufferedLen(); got != 0 {
		t.Fatalf("oversized enqueue buffered %d bytes, want 0", got)
	}
	buf := make([]byte, 4)
	if n, handled := manager.tryRead(buf, &userserver.User{Name: "oversize-test"}, nil); handled || n != 0 {
		t.Fatalf("oversized enqueue exposed a prefix: handled=%v bytes=%q", handled, buf[:n])
	}
}

func TestOutputManagerCommittedPartialReadSurvivesGenerationFlipAndCancellation(t *testing.T) {
	manager := &outputManager{}
	manager.configure(outputManagerConfig{bufferMaxBytes: 16}, handlerTestLogger)
	manager.enable()

	ctx, cancel := context.WithCancel(context.Background())
	var activeGeneration atomic.Uint64
	activeGeneration.Store(1)
	payload := append([]byte("abc"), protocol.MessageDelimiter)
	payload = append(payload, []byte("stale")...)
	payload = append(payload, protocol.MessageDelimiter)
	if err := manager.enqueue(ctx, 1, payload, activeGeneration.Load); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	buf := make([]byte, 2)
	n, handled := manager.tryRead(buf, &userserver.User{Name: "commit-test"}, func(generation uint64) bool {
		return generation != activeGeneration.Load()
	})
	if !handled || string(buf[:n]) != "ab" {
		t.Fatalf("first read = handled %v, payload %q; want ab", handled, buf[:n])
	}
	if got := manager.buffer.generation; got != 1 {
		t.Fatalf("committed remainder generation = %d, want 1", got)
	}

	activeGeneration.Store(2)
	cancel()
	buf = make([]byte, 16)
	n, handled = manager.tryRead(buf, &userserver.User{Name: "commit-test"}, func(generation uint64) bool {
		return generation != activeGeneration.Load()
	})
	want := append([]byte{'c'}, protocol.MessageDelimiter)
	if !handled || !bytes.Equal(buf[:n], want) {
		t.Fatalf("committed record suffix = handled %v, payload %q; want %q", handled, buf[:n], want)
	}
	if got := manager.bufferedLen(); got != 0 {
		t.Fatalf("stale records remain buffered after completing prefix: %d bytes", got)
	}
}

func TestDefaultOutputBufferAdmitsMaximumFormattedLine(t *testing.T) {
	manager := &outputManager{}
	manager.enable()
	writer := NewNetworkWriter(context.Background(), nil, nil, "testhost", false, false, 1, nil, handlerTestLogger)
	writer.enqueueOutput = manager.enqueue

	line := bytes.Repeat([]byte("x"), 1<<20)
	if err := writer.WriteLineData(line, 1, "max-line.log"); err != nil {
		t.Fatalf("maximum-length line: %v", err)
	}
	if got := manager.bufferedLen(); got <= len(line) || got > config.DefaultOutputBufferMaxBytes {
		t.Fatalf("formatted payload bytes = %d, want (%d, %d]", got, len(line), config.DefaultOutputBufferMaxBytes)
	}
}

func TestOutputBufferConfigRejectsCapBelowMaximumLineEnvelope(t *testing.T) {
	err := validateOutputBufferConfig(&config.ServerConfig{
		MaxLineLength:        1024,
		OutputBufferMaxBytes: 1024,
	})
	if err == nil {
		t.Fatal("validateOutputBufferConfig accepted a cap with no room for framing")
	}
}

func TestOutputManagerCoalescesAndReleasesManyTinyPayloads(t *testing.T) {
	manager := &outputManager{}
	manager.configure(outputManagerConfig{bufferMaxBytes: 4096}, handlerTestLogger)
	manager.enable()

	for i := 0; i < 4096; i++ {
		if err := manager.enqueue(context.Background(), 1, []byte{'x'}, nil); err != nil {
			t.Fatalf("enqueue tiny payload %d: %v", i, err)
		}
	}
	manager.mu.Lock()
	if got := len(manager.queue); got != 1 {
		manager.mu.Unlock()
		t.Fatalf("queue descriptors = %d, want 1 coalesced descriptor", got)
	}
	if got := manager.bufferedEntries; got != 1 {
		manager.mu.Unlock()
		t.Fatalf("buffered entries = %d, want 1", got)
	}
	if got := manager.retainedBytes; got > 4096 {
		manager.mu.Unlock()
		t.Fatalf("retained payload backing = %d, exceeds 4096-byte cap", got)
	}
	manager.mu.Unlock()

	buf := make([]byte, 257)
	for manager.bufferedLen() > 0 {
		if n, handled := manager.tryRead(buf, &userserver.User{Name: "tiny-test"}, nil); !handled || n == 0 {
			t.Fatalf("tiny payload drain stalled: handled=%v n=%d", handled, n)
		}
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.queue != nil || manager.buffer.payload != nil || manager.bufferedEntries != 0 || manager.retainedBytes != 0 {
		t.Fatalf("drained queue retained state: queue len/cap=%d/%d buffer=%d entries=%d retained=%d",
			len(manager.queue), cap(manager.queue), len(manager.buffer.payload), manager.bufferedEntries,
			manager.retainedBytes)
	}
}

func TestOutputManagerBackpressuresOnDescriptorLimit(t *testing.T) {
	manager := &outputManager{}
	manager.configure(outputManagerConfig{
		bufferMaxBytes:    128,
		readRetryInterval: time.Millisecond,
	}, handlerTestLogger)
	manager.enable()

	// Alternating generations prevent coalescing. The minimum metadata budget
	// permits sixteen descriptors even though payload-byte capacity remains.
	for i := 0; i < 16; i++ {
		mustEnqueueOutput(t, manager, uint64(i%2+1), []byte{'a'})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := manager.enqueue(ctx, 1, []byte{'c'}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("seventeenth descriptor enqueue error = %v, want deadline exceeded", err)
	}
	if got := manager.bufferedLen(); got != 16 {
		t.Fatalf("buffered payload bytes = %d, want 16", got)
	}
}

func TestOutputManagerCompactsDescriptorStorageWhileDraining(t *testing.T) {
	manager := &outputManager{}
	manager.configure(outputManagerConfig{bufferMaxBytes: 4096}, handlerTestLogger)
	manager.enable()
	for i := 0; i < 64; i++ {
		mustEnqueueOutput(t, manager, uint64(i%2+1), []byte{'x'})
	}
	manager.mu.Lock()
	initialCapacity := cap(manager.queue)
	manager.mu.Unlock()

	buf := make([]byte, 1)
	for i := 0; i < 52; i++ {
		if n, handled := manager.tryRead(buf, &userserver.User{Name: "compact-test"}, nil); !handled || n != 1 {
			t.Fatalf("drain %d = handled %v, n %d", i, handled, n)
		}
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if got := cap(manager.queue); got >= initialCapacity {
		t.Fatalf("queue capacity did not compact: initial %d, current %d", initialCapacity, got)
	}
	if got := manager.bufferedEntries; got != 12 {
		t.Fatalf("buffered entries = %d, want 12", got)
	}
}

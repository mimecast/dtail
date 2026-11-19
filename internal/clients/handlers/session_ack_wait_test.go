package handlers

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/protocol"
)

// blockingPayloadLogger blocks every payload write until release is
// closed, as the client logger does while the interactive output is
// paused. started is closed once the first payload write began.
type blockingPayloadLogger struct {
	clientlog.NopLogger
	started     chan struct{}
	startedOnce sync.Once
	release     chan struct{}
}

func newBlockingPayloadLogger() *blockingPayloadLogger {
	return &blockingPayloadLogger{started: make(chan struct{}), release: make(chan struct{})}
}

func (l *blockingPayloadLogger) RawBytes([]byte) {
	l.startedOnce.Do(func() { close(l.started) })
	<-l.release
}

// sessionStartStream is what a server sends when it queued the SESSION
// START acknowledgement behind the first payload frame: the capabilities
// frame, a payload line and the acknowledgement.
func sessionStartStream() []byte {
	var stream bytes.Buffer
	stream.Write(capabilitiesFrame())
	protocol.EscapedControlFraming.EncodePayload(&stream, []byte("first line\n"))
	protocol.EscapedControlFraming.EncodeControl(&stream, protocol.HiddenSessionStartOKPrefix+" 1")
	return stream.Bytes()
}

// TestWaitForSessionAckBehindBlockedPayload: the acknowledgement is queued
// behind a payload frame whose write blocks (paused output) for longer than
// the acknowledgement timeout. The time the handler is blocked delivering
// output does not count against the timeout, so the acknowledgement read
// after the output resumed is returned.
func TestWaitForSessionAckBehindBlockedPayload(t *testing.T) {
	const timeout = 200 * time.Millisecond
	logger := newBlockingPayloadLogger()
	handler := NewClientHandler("server", logger)
	defer handler.Shutdown()

	written := make(chan struct{})
	go func() {
		defer close(written)
		_, _ = handler.Write(sessionStartStream())
	}()
	<-logger.started
	go func() {
		time.Sleep(3 * timeout)
		close(logger.release)
	}()

	start := time.Now()
	ack, ok := handler.WaitForSessionAck(timeout, 0)
	if !ok {
		t.Fatalf("no session acknowledgement after %v, the blocked payload write counted against the timeout",
			time.Since(start))
	}
	if ack.Action != "start" || ack.Generation != 1 {
		t.Fatalf("ack = %+v, want start 1", ack)
	}
	<-written
}

// TestWaitForSessionAckTimesOutWhileIdle: a server that never answers still
// times the wait out after the timeout, as the handler is not busy.
func TestWaitForSessionAckTimesOutWhileIdle(t *testing.T) {
	const timeout = 200 * time.Millisecond
	handler := NewClientHandler("server", clientlog.NopLogger{})
	defer handler.Shutdown()

	start := time.Now()
	if ack, ok := handler.WaitForSessionAck(timeout, 0); ok {
		t.Fatalf("ack = %+v, want none", ack)
	}
	if elapsed := time.Since(start); elapsed < timeout || elapsed > 10*timeout {
		t.Fatalf("waited %v, want about %v", elapsed, timeout)
	}
}

// TestWaitForSessionAckCountsIdleTimeAroundBlockedWrite: idle time before
// and after a blocked payload write adds up; once it reaches the timeout
// the wait ends without an acknowledgement.
func TestWaitForSessionAckCountsIdleTimeAroundBlockedWrite(t *testing.T) {
	const timeout = 300 * time.Millisecond
	logger := newBlockingPayloadLogger()
	handler := NewClientHandler("server", logger)
	defer handler.Shutdown()

	var stream bytes.Buffer
	stream.Write(capabilitiesFrame())
	protocol.EscapedControlFraming.EncodePayload(&stream, []byte("line\n"))
	written := make(chan struct{})
	go func() {
		defer close(written)
		time.Sleep(timeout / 2)
		_, _ = handler.Write(stream.Bytes())
	}()
	go func() {
		<-logger.started
		time.Sleep(2 * timeout)
		close(logger.release)
	}()

	start := time.Now()
	if ack, ok := handler.WaitForSessionAck(timeout, 0); ok {
		t.Fatalf("ack = %+v, want none", ack)
	}
	// About timeout/2 idle, 2*timeout blocked, timeout/2 idle.
	if elapsed := time.Since(start); elapsed < 2*timeout+timeout/2 {
		t.Fatalf("waited %v, want the blocked %v not counted", elapsed, 2*timeout)
	}
	<-written
}

// TestWaitForSessionAckEndsWithSession: a wait behind a blocked write
// still ends when the session ends.
func TestWaitForSessionAckEndsWithSession(t *testing.T) {
	const timeout = 100 * time.Millisecond
	logger := newBlockingPayloadLogger()
	handler := NewClientHandler("server", logger)
	defer close(logger.release)

	go func() { _, _ = handler.Write(sessionStartStream()) }()
	<-logger.started
	go func() {
		time.Sleep(3 * timeout)
		handler.Shutdown()
	}()
	if ack, ok := handler.WaitForSessionAck(timeout, 0); ok {
		t.Fatalf("ack = %+v, want none", ack)
	}
}

// TestWaitForSessionAckCappedWhileBlocked: with a maxWait, a wait behind a
// payload write that stays blocked ends after maxWait (wall clock), while
// the same wait without one goes on (TestWaitForSessionAckBehindBlockedPayload).
func TestWaitForSessionAckCappedWhileBlocked(t *testing.T) {
	const timeout, maxWait = 100 * time.Millisecond, 400 * time.Millisecond
	logger := newBlockingPayloadLogger()
	handler := NewClientHandler("server", logger)
	defer handler.Shutdown()

	written := make(chan struct{})
	go func() {
		defer close(written)
		_, _ = handler.Write(sessionStartStream())
	}()
	<-logger.started

	start := time.Now()
	if ack, ok := handler.WaitForSessionAck(timeout, maxWait); ok {
		t.Fatalf("ack = %+v, want none while the write is blocked", ack)
	}
	if elapsed := time.Since(start); elapsed < maxWait || elapsed > 5*maxWait {
		t.Fatalf("waited %v, want about %v", elapsed, maxWait)
	}
	close(logger.release)
	<-written
	// The acknowledgement read after the write is still queued.
	if ack, ok := handler.WaitForSessionAck(0, 0); !ok || ack.Generation != 1 {
		t.Fatalf("ack = %+v, %v after the write, want start 1", ack, ok)
	}
}

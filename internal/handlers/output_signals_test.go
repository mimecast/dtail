package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/protocol"
)

func TestBaseHandlerReadWakesWhenOutputArrives(t *testing.T) {
	handler := newReadTestHandler()
	handler.output.enable()

	type readResult struct {
		data []byte
		err  error
	}
	result := make(chan readResult, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		buf := make([]byte, 64)
		n, err := handler.Read(buf)
		result <- readResult{data: append([]byte(nil), buf[:n]...), err: err}
	}()

	<-started
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for !handler.readActive.Load() {
		select {
		case <-deadline.C:
			t.Fatal("Read did not enter its blocking path")
		default:
			runtime.Gosched()
		}
	}
	select {
	case got := <-result:
		t.Fatalf("Read returned before output arrived: (%q, %v)", got.data, got.err)
	default:
	}
	mustEnqueueOutput(t, &handler.output, 0, []byte("arrived"))

	select {
	case got := <-result:
		if got.err != nil || string(got.data) != "arrived" {
			t.Fatalf("Read() = (%q, %v), want (%q, nil)", got.data, got.err, "arrived")
		}
	case <-time.After(time.Second):
		t.Fatal("Read did not wake after output arrived")
	}
}

func TestOutputManagerFlushWaitsForSlowReaderCompletion(t *testing.T) {
	manager := &outputManager{}
	manager.configure(outputManagerConfig{flushTimeout: time.Second}, handlerTestLogger)
	manager.enable()
	mustEnqueueOutput(t, manager, 0, []byte("abcdefgh"))

	flushDone := make(chan error, 1)
	go func() {
		flushDone <- manager.flush(context.Background(), nil)
	}()

	buf := make([]byte, 4)
	if n, handled := manager.tryRead(buf, nil, nil); !handled || n != len(buf) {
		t.Fatalf("first tryRead = (%d, %v), want (%d, true)", n, handled, len(buf))
	}
	select {
	case err := <-flushDone:
		t.Fatalf("flush returned before the partial payload completed: %v", err)
	default:
	}

	if n, handled := manager.tryRead(buf, nil, nil); !handled || string(buf[:n]) != "efgh" {
		t.Fatalf("second tryRead = (%d, %v, %q), want payload %q", n, handled, buf[:n], "efgh")
	}
	select {
	case err := <-flushDone:
		if err != nil {
			t.Fatalf("flush returned error after drain: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("flush did not observe reader completion")
	}
}

func TestOutputManagerFlushEmptyReturnsImmediately(t *testing.T) {
	manager := &outputManager{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.flush(ctx, nil); err != nil {
		t.Fatalf("empty flush returned error: %v", err)
	}
}

func TestOutputManagerFlushTimesOutWithRemainingBytes(t *testing.T) {
	manager := &outputManager{}
	manager.configure(outputManagerConfig{flushTimeout: 5 * time.Millisecond}, handlerTestLogger)
	manager.enable()
	mustEnqueueOutput(t, manager, 0, []byte("blocked"))

	err := manager.flush(context.Background(), nil)
	if !errors.Is(err, errOutputFlushTimeout) {
		t.Fatalf("flush error = %v, want %v", err, errOutputFlushTimeout)
	}
	if !strings.Contains(err.Error(), "7 bytes remain") {
		t.Fatalf("flush error %q does not report remaining bytes", err)
	}
}

func TestOutputManagerFlushStopsOnCancellation(t *testing.T) {
	manager := &outputManager{}
	manager.enable()
	mustEnqueueOutput(t, manager, 0, []byte("blocked"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := manager.flush(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("flush error = %v, want context cancellation", err)
	}
}

func TestOutputManagerStateSignalsCoalesce(t *testing.T) {
	manager := &outputManager{}
	changed, _ := manager.readWait()

	manager.enable()
	mustEnqueueOutput(t, manager, 0, []byte("a"))
	mustEnqueueOutput(t, manager, 0, []byte("b"))
	manager.signalEOF(manager.currentEpoch())
	manager.signalEOF(manager.currentEpoch())

	select {
	case <-changed:
	default:
		t.Fatal("state waiter was not released by repeated output signals")
	}
	select {
	case <-changed:
		t.Fatal("repeated state signals did not coalesce")
	default:
	}

	current, _ := manager.readWait()
	if current != changed {
		t.Fatal("state notification channel was replaced")
	}
}

func TestOutputManagerEmptyEOFPublishesQuietPeriodWake(t *testing.T) {
	manager := &outputManager{}
	manager.configure(outputManagerConfig{eofAckQuietPeriod: time.Second}, handlerTestLogger)
	manager.enable()
	manager.signalEOF(manager.currentEpoch())

	changed, eofWait := manager.readWait()
	if eofWait != 0 {
		t.Fatalf("quiet deadline existed before the reader observed EOF: %v", eofWait)
	}
	if n, handled := manager.tryRead(make([]byte, 1), nil, nil); n != 0 || handled {
		t.Fatalf("empty EOF tryRead = (%d, %v), want (0, false)", n, handled)
	}

	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("reader was not woken to observe the EOF quiet-period deadline")
	}
	_, eofWait = manager.readWait()
	if eofWait <= 0 {
		t.Fatalf("quiet deadline after EOF observation = %v, want positive", eofWait)
	}
}

func TestBaseHandlerFlushWaitsForPartialReadBuffer(t *testing.T) {
	handler := newReadTestHandler()
	message := ".hidden " + strings.Repeat("x", 64)
	want := append([]byte(message), protocol.MessageDelimiter)
	handler.serverMessages <- message

	first := make([]byte, 8)
	n, err := handler.Read(first)
	if err != nil {
		t.Fatalf("initial Read() error = %v", err)
	}

	flushDone := make(chan error, 1)
	go func() {
		flushDone <- handler.flushContext(context.Background())
	}()

	pumpDone := make(chan []byte, 1)
	go func() {
		got := append([]byte(nil), first[:n]...)
		buf := make([]byte, 8)
		for len(got) < len(want) {
			readN, readErr := handler.Read(buf)
			if readErr != nil {
				pumpDone <- nil
				return
			}
			got = append(got, buf[:readN]...)
		}
		// The next Read confirms to flush that the previous chunk was accepted
		// by the consumer. It remains blocked until the test closes the handler.
		_, _ = handler.Read(buf)
		pumpDone <- got
	}()

	select {
	case flushErr := <-flushDone:
		if flushErr != nil {
			t.Fatalf("flush returned error: %v", flushErr)
		}
	case <-time.After(time.Second):
		t.Fatal("flush did not complete after the reader drained the remainder")
	}
	handler.done.Shutdown()

	select {
	case got := <-pumpDone:
		if !bytes.Equal(got, want) {
			t.Fatalf("drained payload = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("reader did not stop after handler shutdown")
	}
}

func TestBaseHandlerFlushWaitsForExactReadDeliveryConfirmation(t *testing.T) {
	handler := newReadTestHandler()
	message := ".exact"
	want := append([]byte(message), protocol.MessageDelimiter)
	handler.serverMessages <- message

	buf := make([]byte, len(want))
	n, err := handler.Read(buf)
	if err != nil || !bytes.Equal(buf[:n], want) {
		t.Fatalf("Read() = (%q, %v), want (%q, nil)", buf[:n], err, want)
	}

	flushDone := make(chan error, 1)
	go func() {
		flushDone <- handler.flushContext(context.Background())
	}()
	readerDone := make(chan error, 1)
	go func() {
		_, readErr := handler.Read(buf)
		readerDone <- readErr
	}()

	select {
	case flushErr := <-flushDone:
		if flushErr != nil {
			t.Fatalf("flush returned error: %v", flushErr)
		}
	case <-time.After(time.Second):
		t.Fatal("flush did not wait for the next reader call to confirm delivery")
	}
	handler.done.Shutdown()
	select {
	case readErr := <-readerDone:
		if !errors.Is(readErr, io.EOF) {
			t.Fatalf("reader error = %v, want EOF", readErr)
		}
	case <-time.After(time.Second):
		t.Fatal("reader did not stop after handler shutdown")
	}
}

func TestAttachedOutputReaderShutdownWaitsBeforeFirstRead(t *testing.T) {
	handler := newReadTestHandler()
	// Buffer the request so the test can observe that flush crossed its
	// attachment check before starting Read, without stealing the reader-owned
	// acknowledgement.
	handler.flushRequests = make(chan chan struct{}, 1)
	handler.ackCloseReceived = make(chan struct{})
	handler.AttachOutputReader()
	handler.maprMessages <- "final aggregate"

	shutdownDone := make(chan struct{})
	go func() {
		handler.shutdown(context.Background())
		close(shutdownDone)
	}()
	var ackOnce sync.Once
	ackClose := func() { ackOnce.Do(func() { close(handler.ackCloseReceived) }) }
	defer ackClose()

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for len(handler.flushRequests) == 0 {
		if len(handler.serverMessages) > 0 {
			t.Fatal("shutdown queued close sync before the attached reader started")
		}
		select {
		case <-deadline.C:
			t.Fatal("shutdown flush did not wait for the attached reader")
		default:
			runtime.Gosched()
		}
	}

	buf := make([]byte, 256)
	n, err := handler.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "final aggregate") {
		t.Fatalf("first Read() = (%q, %v), want queued aggregate", buf[:n], err)
	}

	type readResult struct {
		data []byte
		err  error
	}
	readerDone := make(chan readResult, 1)
	go func() {
		readN, readErr := handler.Read(buf)
		readerDone <- readResult{data: append([]byte(nil), buf[:readN]...), err: readErr}
	}()
	select {
	case result := <-readerDone:
		if result.err != nil {
			t.Fatalf("close-sync Read() error = %v", result.err)
		}
		wantSync := append([]byte(".syn close connection"), protocol.MessageDelimiter)
		if !bytes.Equal(result.data, wantSync) {
			t.Fatalf("second Read() = %q, want %q", result.data, wantSync)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not send close sync after reader completion")
	}

	ackClose()
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after close acknowledgement")
	}
}

func TestBaseHandlerFlushTimeoutIsClientVisibleBeforeCloseSync(t *testing.T) {
	handler := newReadTestHandler()
	handler.output.configure(outputManagerConfig{flushTimeout: 5 * time.Millisecond}, handlerTestLogger)
	handler.readerSeen.Store(true)
	handler.serverMessages <- "undrained"

	err := handler.flushContext(context.Background())
	if !errors.Is(err, errHandlerFlushTimeout) {
		t.Fatalf("flush error = %v, want %v", err, errHandlerFlushTimeout)
	}
	handler.reportFlushError(0, err)
	handler.serverMessages <- ".syn close connection"

	buf := make([]byte, 512)
	n, readErr := handler.Read(buf)
	if readErr != nil {
		t.Fatalf("Read() error = %v", readErr)
	}
	if got := string(buf[:n]); !strings.Contains(got, errHandlerFlushTimeout.Error()) {
		t.Fatalf("first message after timeout = %q, want client-visible flush error", got)
	}
}

func TestBaseHandlerBlockedReadKeepsFlushErrorBeforeCloseSync(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previousProcs) })

	handler := newReadTestHandler()
	type readResult struct {
		data []byte
		err  error
	}
	result := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 512)
		n, err := handler.Read(buf)
		result <- readResult{data: append([]byte(nil), buf[:n]...), err: err}
	}()

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for !handler.readActive.Load() {
		select {
		case <-deadline.C:
			t.Fatal("Read did not enter its blocking path")
		default:
			runtime.Gosched()
		}
	}

	flushErr := errors.New("flush deadline reached")
	handler.reportFlushError(0, flushErr)
	handler.serverMessages <- ".syn close connection"

	select {
	case got := <-result:
		if got.err != nil || !strings.Contains(string(got.data), flushErr.Error()) {
			t.Fatalf("first Read() = (%q, %v), want flush error", got.data, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Read did not receive flush error")
	}

	buf := make([]byte, 512)
	n, err := handler.Read(buf)
	if err != nil {
		t.Fatalf("close-sync Read() error = %v", err)
	}
	wantSync := append([]byte(".syn close connection"), protocol.MessageDelimiter)
	if !bytes.Equal(buf[:n], wantSync) {
		t.Fatalf("second Read() = %q, want %q", buf[:n], wantSync)
	}
}

func TestShutdownTimeoutDrainsQueuedAggregateBeforeReaderOwnedClose(t *testing.T) {
	handler := newReadTestHandler()
	handler.output.configure(outputManagerConfig{flushTimeout: 5 * time.Millisecond}, handlerTestLogger)
	handler.ackCloseReceived = make(chan struct{})
	handler.AttachOutputReader()
	handler.maprMessages <- "queued aggregate"

	shutdownDone := make(chan struct{})
	go func() {
		handler.shutdown(context.Background())
		close(shutdownDone)
	}()
	var ackOnce sync.Once
	ackClose := func() { ackOnce.Do(func() { close(handler.ackCloseReceived) }) }
	defer ackClose()

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for !handler.closeSyncRequested.Load() {
		select {
		case <-deadline.C:
			t.Fatal("shutdown did not request reader-owned close after forced flush timeout")
		default:
			runtime.Gosched()
		}
	}
	if len(handler.serverMessages) != 0 {
		t.Fatal("attached shutdown queued close on normal serverMessages path")
	}

	type readResult struct {
		data []byte
		err  error
	}
	readNext := func() []byte {
		t.Helper()
		result := make(chan readResult, 1)
		go func() {
			buf := make([]byte, 512)
			n, err := handler.Read(buf)
			result <- readResult{data: append([]byte(nil), buf[:n]...), err: err}
		}()
		select {
		case got := <-result:
			if got.err != nil {
				t.Fatalf("Read() error = %v", got.err)
			}
			return got.data
		case <-time.After(time.Second):
			t.Fatal("slow reader did not resume")
			return nil
		}
	}

	if got := string(readNext()); !strings.Contains(got, errHandlerFlushTimeout.Error()) {
		t.Fatalf("first message = %q, want flush timeout", got)
	}
	if got := string(readNext()); !strings.Contains(got, "queued aggregate") {
		t.Fatalf("second message = %q, want queued aggregate", got)
	}
	wantSync := append([]byte(".syn close connection"), protocol.MessageDelimiter)
	if got := readNext(); !bytes.Equal(got, wantSync) {
		t.Fatalf("third message = %q, want %q", got, wantSync)
	}

	ackClose()
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after close acknowledgement")
	}
}

func TestFlushRequestReceiptForcesFreshDrainBeforeTerminalClose(t *testing.T) {
	handler := newReadTestHandler()
	handler.flushRequests = make(chan chan struct{}, 1)
	handler.AttachOutputReader()
	flushAck := make(chan struct{})
	handler.flushRequests <- flushAck

	// Model Read reaching the barrier after its queue scan. Receipt must not
	// acknowledge immediately because the flush deadline can win and publish
	// its error and terminal request at this exact boundary.
	if result, handled := handler.tryReadQueued(); result.kind != outputReadRetry || !handled {
		t.Fatalf("barrier receipt = (%v, %v), want (retry, true) fresh-pass request",
			result.kind, handled)
	}
	select {
	case <-flushAck:
		t.Fatal("flush was acknowledged without a fresh queue pass")
	default:
	}

	handler.reportFlushError(0, errHandlerFlushTimeout)
	handler.maprMessages <- "payload published at barrier boundary"
	handler.requestCloseSync(context.Background())

	buf := make([]byte, 512)
	n, err := handler.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), errHandlerFlushTimeout.Error()) {
		t.Fatalf("first Read() = (%q, %v), want timeout error", buf[:n], err)
	}
	select {
	case <-flushAck:
		t.Fatal("flush was acknowledged before the late payload drained")
	default:
	}

	n, err = handler.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "payload published at barrier boundary") {
		t.Fatalf("second Read() = (%q, %v), want late payload", buf[:n], err)
	}
	select {
	case <-flushAck:
		t.Fatal("flush was acknowledged before the payload delivery boundary")
	default:
	}

	n, err = handler.Read(buf)
	wantSync := append([]byte(".syn close connection"), protocol.MessageDelimiter)
	if err != nil || !bytes.Equal(buf[:n], wantSync) {
		t.Fatalf("third Read() = (%q, %v), want %q", buf[:n], err, wantSync)
	}
	select {
	case <-flushAck:
	default:
		t.Fatal("pending flush barrier was not acknowledged before terminal close")
	}
}

func TestOutputManagerWaitForEOFAckStopsOnCancellation(t *testing.T) {
	manager := &outputManager{}
	manager.enable()
	ctx, cancel := context.WithCancel(context.Background())

	result := make(chan bool, 1)
	go func() {
		result <- manager.waitForEOFAck(ctx, time.Hour)
	}()
	cancel()

	select {
	case acknowledged := <-result:
		if acknowledged {
			t.Fatal("cancelled EOF wait reported acknowledgement")
		}
	case <-time.After(time.Second):
		t.Fatal("EOF acknowledgement wait did not stop promptly on cancellation")
	}
}

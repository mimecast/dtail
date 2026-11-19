package connectors

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/protocol"
)

// pausedOutputLogger blocks every payload write until release is closed,
// as the client logger does while the interactive output is paused.
type pausedOutputLogger struct {
	clientlog.NopLogger
	started     chan struct{}
	startedOnce sync.Once
	release     chan struct{}
}

func newPausedOutputLogger() *pausedOutputLogger {
	return &pausedOutputLogger{started: make(chan struct{}), release: make(chan struct{})}
}

func (l *pausedOutputLogger) RawBytes([]byte) {
	l.startedOnce.Do(func() { close(l.started) })
	<-l.release
}

// releaseAfter resumes the paused output d after its first payload write.
func (l *pausedOutputLogger) releaseAfter(d time.Duration) {
	go func() {
		<-l.started
		time.Sleep(d)
		close(l.release)
	}()
}

// serveSession plays a server that queues its acknowledgement of a SESSION
// command behind a payload frame (or, with an empty ack, never answers):
// it reads the command from handler, checks that it starts with want, and
// writes the capabilities frame, a payload line and the acknowledgement.
func serveSession(t *testing.T, handler handlers.Handler, want, ack string) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p := make([]byte, 64*1024)
		n, err := handler.Read(p)
		if command := decodeCommandFrame(string(p[:n])); err != nil || !strings.HasPrefix(command, want) {
			t.Errorf("read %q (%q), %v, want %q", p[:n], command, err, want)
			return
		}
		var stream bytes.Buffer
		protocol.EscapedControlFraming.EncodeControl(&stream, protocol.HiddenCapabilitiesPrefix)
		protocol.EscapedControlFraming.EncodePayload(&stream, []byte("first line\n"))
		if ack != "" {
			protocol.EscapedControlFraming.EncodeControl(&stream, ack)
		}
		_, _ = handler.Write(stream.Bytes())
	}()
	return done
}

// decodeCommandFrame returns the command of a client command frame
// ("protocol <version> base64 <command>;"), or "" if frame is none.
func decodeCommandFrame(frame string) string {
	encoded, ok := strings.CutPrefix(frame, "protocol "+protocol.ProtocolCompat+" base64 ")
	if !ok {
		return ""
	}
	command, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(encoded, ";"))
	if err != nil {
		return ""
	}
	return string(command)
}

// shortenUpdateAckMaxWait sets sessionUpdateAckMaxWait to d for the test.
func shortenUpdateAckMaxWait(t *testing.T, d time.Duration) {
	t.Helper()
	saved := sessionUpdateAckMaxWait
	sessionUpdateAckMaxWait = d
	t.Cleanup(func() { sessionUpdateAckMaxWait = saved })
}

// TestSessionStartAckBehindPausedPayload: the server's SESSION START
// acknowledgement is queued behind a payload frame, and the output is
// paused for longer than defaultSessionAckTimeout and the control loop's
// cap (sessionUpdateAckMaxWait, shortened), so the real client handler
// does not read it meanwhile. The initial start, which has no wall-clock
// cap, must wait for it rather than fail with ErrSessionAckTimeout, and
// commit the session. A :reload replacing the start spec meanwhile does
// not wait for the start without bound: it gets ErrSessionStartPending
// after the cap.
func TestSessionStartAckBehindPausedPayload(t *testing.T) {
	const maxWait = 300 * time.Millisecond
	shortenUpdateAckMaxWait(t, maxWait)
	logger := newPausedOutputLogger()
	handler := handlers.NewClientHandler("srv1", logger)
	defer handler.Shutdown()
	served := serveSession(t, handler, "SESSION START ", protocol.HiddenSessionStartOKPrefix+" 1")
	logger.releaseAfter(defaultSessionAckTimeout + time.Second)

	var state committedSessionState
	startErr := make(chan error, 1)
	go func() {
		startErr <- startInitialSession("srv1", handler, &state, startSpecInitial, logging.NopLogger{})
	}()
	<-logger.started
	begin := time.Now()
	replaced, err := state.replaceStartSpec(startSpecReloaded)
	if replaced || !errors.Is(err, ErrSessionStartPending) {
		t.Fatalf("replaceStartSpec() = %v, %v during the start, want ErrSessionStartPending", replaced, err)
	}
	if elapsed := time.Since(begin); elapsed < maxWait || elapsed > 5*maxWait {
		t.Fatalf("replaceStartSpec() waited %v, want about %v", elapsed, maxWait)
	}
	if err := <-startErr; err != nil {
		t.Fatalf("startInitialSession() error = %v, want the start acknowledged after the pause", err)
	}
	if spec, generation, committed := state.snapshot(); !committed || generation != 1 || spec.Regex != "ERROR" {
		t.Fatalf("committed %v generation %d spec %+v, want the initial spec at 1", committed, generation, spec)
	}
	<-served
}

// TestSessionUnansweredTimesOut: a server that answers a SESSION request
// with payload only, never with an acknowledgement, still fails it with
// ErrSessionAckTimeout once the output runs: only the time the payload
// write was blocked is not counted.
func TestSessionUnansweredTimesOut(t *testing.T) {
	const timeout = 200 * time.Millisecond
	logger := newPausedOutputLogger()
	handler := handlers.NewClientHandler("srv1", logger)
	defer handler.Shutdown()
	served := serveSession(t, handler, "SESSION START ", "")
	logger.releaseAfter(timeout)

	var state committedSessionState
	start := time.Now()
	err := applySessionSpec("srv1", handler, &state, startSpecInitial, timeout, logging.NopLogger{})
	if !errors.Is(err, ErrSessionAckTimeout) {
		t.Fatalf("applySessionSpec() error = %v, want ErrSessionAckTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 20*timeout {
		t.Fatalf("timed out after %v, want about %v plus the pause", elapsed, 2*timeout)
	}
	if _, _, committed := state.snapshot(); committed {
		t.Fatal("session committed without an acknowledgement")
	}
	<-served
}

// TestSessionUpdateAckCappedWhileBlocked: a :reload's SESSION UPDATE whose
// acknowledgement the handler never reads, as its payload write stays
// blocked (a host key prompt pausing the output, which only the control
// loop running the reload would answer), ends with ErrSessionAckTimeout
// after sessionUpdateAckMaxWait (shortened) instead of waiting forever.
// The committed session is left as it was.
func TestSessionUpdateAckCappedWhileBlocked(t *testing.T) {
	const maxWait = 300 * time.Millisecond
	shortenUpdateAckMaxWait(t, maxWait)
	logger := newPausedOutputLogger()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(logger.release) }) }
	defer release()
	handler := handlers.NewClientHandler("srv1", logger)
	defer handler.Shutdown()
	served := serveSession(t, handler, "SESSION UPDATE 2 ", protocol.HiddenSessionUpdateOKPrefix+" 2")

	var state committedSessionState
	state.commit(startSpecInitial, 1)
	start := time.Now()
	err := applySessionSpec("srv1", handler, &state, startSpecReloaded, 100*time.Millisecond, logging.NopLogger{})
	if !errors.Is(err, ErrSessionAckTimeout) {
		t.Fatalf("applySessionSpec() error = %v, want ErrSessionAckTimeout", err)
	}
	if elapsed := time.Since(start); elapsed < maxWait || elapsed > 5*maxWait {
		t.Fatalf("update waited %v, want about %v", elapsed, maxWait)
	}
	if spec, generation, _ := state.snapshot(); generation != 1 || spec.Regex != "ERROR" {
		t.Fatalf("committed generation %d spec %+v, want the initial spec at 1", generation, spec)
	}
	release()
	<-served
}

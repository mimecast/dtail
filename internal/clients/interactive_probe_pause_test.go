package clients

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	sessionHandlers "github.com/mimecast/dtail/internal/handlers"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	user "github.com/mimecast/dtail/internal/sessionuser"
)

// TestProbeAnswerWhileOutputPaused settles whether a probe answer reaches
// the client while the interactive output is paused with payload still
// flowing (plan constraint 1, recorded in the interactiveProber comment and
// AGENTS.md). It runs a real interactive serverless session (in-process
// dserver handler, real client handler) that cats a large file, pauses the
// output right after the first payload was written once the session start
// was acknowledged (from inside that write, so payload is still pending for
// sure), sends a probe and checks whether its answer arrives within
// pausedWait; then it resumes and requires the answer.
//
// pausedWait only bounds how long the test waits for an answer while
// paused. A case expecting none waits 2 s: under load that can only make a
// late answer look absent, never fail the case. The case expecting an
// answer while paused waits much longer, so that a slow host does not make
// the answer look absent.
//
//   - framed: the payload is framed through the client handler, as from a
//     remote dserver over SSH. The paused writer blocks the handler's Write,
//     so frames behind the pending payload, the probe answer included, are
//     not read until the output resumes: no answer while paused.
//   - direct: serverless mode as the real client runs it: the in-process
//     server writes payload straight to the (gated) output, past the client
//     handler, which only gets control frames, and it logs through the
//     client's (paused) output. Handling the request it logs, which blocks
//     until the output resumes: no answer while paused.
//   - direct, quiet server: the same with an in-process server that logs
//     nowhere. Its answer arrives while paused, which shows that the gate
//     itself does not hold back the control frames: an answer of a probe
//     the server handled before the pause can arrive while paused, a
//     request sent while paused cannot (as AGENTS.md and docs/dev/probe.md
//     say), only because of the server's logging.
func TestProbeAnswerWhileOutputPaused(t *testing.T) {
	for _, tt := range []struct {
		name            string
		direct          bool
		quietServer     bool
		wantWhilePaused bool
	}{
		{name: "framed", direct: false, wantWhilePaused: false},
		{name: "direct", direct: true, wantWhilePaused: false},
		{name: "direct, quiet server", direct: true, quietServer: true, wantWhilePaused: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pausedWait := 2 * time.Second
			if tt.wantWhilePaused {
				pausedWait = 20 * time.Second
			}
			arrivedWhilePaused, answer := runPausedProbeSession(t, tt.direct, tt.quietServer, pausedWait)
			if arrivedWhilePaused != tt.wantWhilePaused {
				t.Fatalf("probe answer arrived while paused = %v, want %v", arrivedWhilePaused, tt.wantWhilePaused)
			}
			if answer.FilesSampled != 1 || answer.Formats["default"].Tables["STATS"] == nil {
				t.Fatalf("answer = %+v, want the STATS table of the sampled file", answer)
			}
		})
	}
}

// pausedProbeSession is the client side of runPausedProbeSession.
type pausedProbeSession struct {
	logger    *pausingTestLogger
	runtime   *clientRuntimeBoundary
	handler   *handlers.ClientHandler
	connector *connectors.Serverless
	paused    chan struct{}
	pauseOnce sync.Once
	// pauseBeforeAck makes pause pause on the first payload write, also
	// before the session start was acknowledged
	// (TestSessionStartWhileOutputPaused).
	pauseBeforeAck bool
}

// pause pauses the logger and the serverless output gate, once, as
// interactivePause.pause does, and signals it. It does nothing until the
// connection committed its SESSION START, i.e. the client handler read the
// server's acknowledgement: the server may queue that acknowledgement
// behind the first payload frames (its session output reads payload before
// server messages), and a paused handler does not read it. The start no
// longer fails for that (the paused time does not count against
// defaultSessionAckTimeout, see TestSessionStartWhileOutputPaused), but it
// is still waiting while paused, and the probe of this test is about a
// started session. A later payload write pauses instead; the file is large
// enough for payload to be pending still. With pauseBeforeAck the first
// payload write pauses.
func (s *pausedProbeSession) pause() {
	if _, _, committed := s.connector.CommittedSession(); !committed && !s.pauseBeforeAck {
		return
	}
	s.pauseOnce.Do(func() {
		s.logger.Pause()
		s.runtime.outputGate.Pause()
		close(s.paused)
	})
}

func (s *pausedProbeSession) resume() {
	s.runtime.outputGate.Resume()
	s.logger.Resume()
}

// runPausedProbeSession runs the session of TestProbeAnswerWhileOutputPaused
// and returns whether the probe answer arrived while the output was paused,
// and the answer. It waits at most pausedWait for an answer while paused.
func runPausedProbeSession(t *testing.T, direct, quietServer bool,
	pausedWait time.Duration) (bool, protocol.ProbeAnswer) {
	t.Helper()
	file := writeProbeTestLog(t, 20000)
	session := newPausedProbeSession(t, file, direct, quietServer)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	go func() {
		defer close(started)
		session.connector.Start(ctx, cancel, nil, nil)
	}()
	defer func() { cancel(); session.resume(); <-started }()

	select {
	case <-session.paused:
	case <-time.After(10 * time.Second):
		t.Fatal("no payload arrived after the session start was acknowledged")
	}
	type result struct {
		answer protocol.ProbeAnswer
		err    error
	}
	results := make(chan result, 1)
	go func() {
		probeCtx, probeCancel := context.WithTimeout(ctx, 20*time.Second)
		defer probeCancel()
		answer, err := session.handler.Probe(probeCtx, protocol.ProbeRequest{ID: "p1", Files: []string{file}})
		results <- result{answer, err}
	}()

	arrivedWhilePaused := false
	var got result
	select {
	case got = <-results:
		arrivedWhilePaused = true
	case <-time.After(pausedWait):
		session.resume()
		got = <-results
	}
	if got.err != nil {
		t.Fatalf("probe error = %v", got.err)
	}
	return arrivedWhilePaused, got.answer
}

// newPausedProbeSession builds the session of runPausedProbeSession. Unless
// quietServer is set, the in-process server logs at info level and above
// through the client's pausable output, as dlog.Server does in a real
// serverless client.
func newPausedProbeSession(t *testing.T, file string, direct, quietServer bool) *pausedProbeSession {
	t.Helper()
	session := &pausedProbeSession{logger: newPausingTestLogger(), paused: make(chan struct{})}
	cfg := config.RuntimeConfig{
		Server: config.NewDefaultServerConfigForTest(),
		Client: &config.ClientConfig{},
		Common: &config.CommonConfig{HostnameOverride: "probehost"},
	}
	var serverLogger logging.Logger = logging.NopLogger{}
	if !quietServer {
		serverLogger = pausedServerLogger{output: session.logger}
	}
	session.runtime = newClientRuntimeBoundary(cfg, NewLoggerDependencies(clientlog.NopLogger{},
		serverLogger, logging.NopLogger{}))
	// The first payload write after the session start was acknowledged
	// pauses the output from inside the write (pause), so the rest of the
	// file is still pending when the probe is sent.
	session.runtime.stdout = func() io.Writer { return firstWritePauses{session: session} }
	session.logger.onRaw = session.pause
	session.handler = handlers.NewClientHandler("local(serverless)", session.logger)

	var factory connectors.ServerlessHandlerFactory = session.runtime
	if !direct {
		factory = framedServerlessFactory{runtime: session.runtime}
	}
	spec := SessionSpec{Mode: omode.CatClient, Files: []string{file}}
	commands, err := spec.Commands()
	if err != nil {
		t.Fatal(err)
	}
	session.connector = connectors.NewServerless("probe-user", session.handler, commands, spec, true,
		factory, logging.NopLogger{})
	return session
}

// pausedServerLogger is the in-process server's logger of
// runPausedProbeSession: info, warnings and errors are written through the
// client's pausable output (blocking while it is paused, without pausing it
// as payload does), debug and trace are off, as with the default log level.
type pausedServerLogger struct {
	logging.NopLogger
	output *pausingTestLogger
}

func (l pausedServerLogger) Info(args ...any) string  { return l.output.log(fmt.Sprint(args...)) }
func (l pausedServerLogger) Warn(args ...any) string  { return l.output.log(fmt.Sprint(args...)) }
func (l pausedServerLogger) Error(args ...any) string { return l.output.log(fmt.Sprint(args...)) }

// firstWritePauses is the serverless direct output of
// runPausedProbeSession: it pauses the session on its first write after
// the session start was acknowledged (pause).
type firstWritePauses struct {
	session *pausedProbeSession
}

func (w firstWritePauses) Write(p []byte) (int, error) {
	w.session.pause()
	return len(p), nil
}

// framedServerlessFactory builds the in-process server handler without the
// serverless direct output, so payload is framed through the client handler
// as from a remote dserver.
type framedServerlessFactory struct {
	runtime *clientRuntimeBoundary
}

func (f framedServerlessFactory) NewServerlessHandler(ctx context.Context,
	userName string) (sessionHandlers.Handler, error) {
	serverUser, err := user.New(userName, "local(serverless)", f.runtime.serverCfg.UserPermissions, nil)
	if err != nil {
		return nil, err
	}
	dependencies := f.runtime.serverlessDependencies()
	dependencies.ServerlessOutput = nil
	return sessionHandlers.NewForUser(ctx, serverUser, dependencies)
}

// writeProbeTestLog writes a log of lines lines in the default MapReduce
// log format (table STATS) and returns its path.
func writeProbeTestLog(t *testing.T, lines int) string {
	t.Helper()
	var sb strings.Builder
	for i := range lines {
		fmt.Fprintf(&sb, "INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=%d|lifetimeConnections=%d\n",
			i%7, i)
	}
	file := filepath.Join(t.TempDir(), "probe.log")
	if err := os.WriteFile(file, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

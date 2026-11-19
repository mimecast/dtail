package clients

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
)

// probeTestHandler is a connection handler that answers probes (or blocks
// until the context ends when block is set) and records the requests: a
// field probe with a STATS table whose field "files" lists the request's
// files, a path listing with the entries of listings for its prefix (none
// for an unknown prefix; listGate, if set, holds a listing back until it
// returns true for its prefix, as a remote server's answer is held back
// while the output is paused). Its session is up (WaitForCapabilities)
// unless connecting is set. Only Done, WaitForCapabilities and Probe are
// implemented; the embedded nil Handler panics on any other call, which the
// prober must not make.
type probeTestHandler struct {
	handlers.Handler
	done     chan struct{}
	block    bool
	err      error
	listings map[string][]protocol.ProbeEntry
	listGate func(prefix string) bool
	// connecting marks a connection whose session is not up yet.
	connecting bool

	mu       sync.Mutex
	requests []protocol.ProbeRequest
	ctxErrs  []error
}

func newProbeTestHandler() *probeTestHandler {
	return &probeTestHandler{done: make(chan struct{})}
}

func (h *probeTestHandler) Done() <-chan struct{} { return h.done }

func (h *probeTestHandler) WaitForCapabilities(time.Duration) bool { return !h.connecting }

func (h *probeTestHandler) Probe(ctx context.Context, request protocol.ProbeRequest) (protocol.ProbeAnswer, error) {
	h.mu.Lock()
	h.requests = append(h.requests, request)
	h.mu.Unlock()
	if h.block {
		<-ctx.Done()
		h.mu.Lock()
		h.ctxErrs = append(h.ctxErrs, ctx.Err())
		h.mu.Unlock()
		return protocol.ProbeAnswer{}, ctx.Err()
	}
	if h.err != nil {
		return protocol.ProbeAnswer{}, h.err
	}
	if request.List != "" {
		return h.answerListing(ctx, request)
	}
	if request.Table != "" {
		answer := tableTestAnswer(request.Table, "field", "caller.go:1", 1)
		answer.ID = request.ID
		return answer, nil
	}
	return protocol.ProbeAnswer{ID: request.ID, DefaultLogFormat: "default", FilesSampled: len(request.Files),
		Formats: map[string]protocol.ProbeFormat{"default": {Tables: map[string]map[string][]string{
			"STATS": {"files": request.Files}}}}}, nil
}

// answerListing answers a path listing once listGate lets it through, or
// fails when ctx ends first.
func (h *probeTestHandler) answerListing(ctx context.Context, request protocol.ProbeRequest) (protocol.ProbeAnswer, error) {
	for h.listGate != nil && !h.listGate(request.List) {
		select {
		case <-ctx.Done():
			return protocol.ProbeAnswer{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	dir, _ := listingDirKey(request.List)
	return protocol.ProbeAnswer{ID: request.ID, Dir: dir, Entries: h.listings[request.List]}, nil
}

// recorded returns the field probe requests the handler got.
func (h *probeTestHandler) recorded() []protocol.ProbeRequest {
	return h.requestsWhere(func(r protocol.ProbeRequest) bool { return r.List == "" })
}

// listed returns the path listing prefixes the handler was asked for.
func (h *probeTestHandler) listed() []string {
	var prefixes []string
	for _, r := range h.requestsWhere(func(r protocol.ProbeRequest) bool { return r.List != "" }) {
		prefixes = append(prefixes, r.List)
	}
	return prefixes
}

func (h *probeTestHandler) requestsWhere(keep func(protocol.ProbeRequest) bool) []protocol.ProbeRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	var requests []protocol.ProbeRequest
	for _, r := range h.requests {
		if keep(r) {
			requests = append(requests, r)
		}
	}
	return requests
}

// probeTestConnector is a connection with a fixed server name and handler;
// the embedded nil Connector panics on any other call.
type probeTestConnector struct {
	connectors.Connector
	server  string
	handler handlers.Handler
}

func (c *probeTestConnector) Server() string            { return c.server }
func (c *probeTestConnector) Handler() handlers.Handler { return c.handler }

// noPayloadLogger fails the test on everything but debug and trace logging:
// the prober must print nothing.
type noPayloadLogger struct {
	clientlog.NopLogger
	mu      sync.Mutex
	printed []string
}

func (l *noPayloadLogger) record(args ...any) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	message := fmt.Sprint(args...)
	l.printed = append(l.printed, message)
	return message
}

func (l *noPayloadLogger) Info(args ...any) string  { return l.record(args...) }
func (l *noPayloadLogger) Warn(args ...any) string  { return l.record(args...) }
func (l *noPayloadLogger) Error(args ...any) string { return l.record(args...) }
func (l *noPayloadLogger) Raw(message string) string {
	return l.record(message)
}
func (l *noPayloadLogger) RawLog(message string) string { return l.record(message) }
func (l *noPayloadLogger) RawBytes(message []byte)      { l.record(string(message)) }

func (l *noPayloadLogger) requireSilent(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.printed) > 0 {
		t.Fatalf("prober printed %q", l.printed)
	}
}

// newProbeTestClient returns a client of connections reading files, with a
// prober (not started) whose logger is returned too.
func newProbeTestClient(files []string, connections ...connectors.Connector) (*baseClient, *noPayloadLogger) {
	logger := &noPayloadLogger{}
	c := &baseClient{
		mu:          newBaseClientMu(),
		sessionSpec: SessionSpec{Mode: omode.TailClient, Files: files},
		connections: connections,
		loggers:     LoggerDependencies{Client: logger},
	}
	c.prober = c.newInteractiveProber()
	return c, logger
}

func probeTestConnection(server string) (*probeTestConnector, *probeTestHandler) {
	handler := newProbeTestHandler()
	return &probeTestConnector{server: server, handler: handler}, handler
}

// TestInteractiveProberFirstRound: the first round, started right away,
// asks the first three connections whose session did not end and whose
// handler probes, with the session's files as read command path words
// (stdin left out), and stores their answers. It prints nothing.
func TestInteractiveProberFirstRound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ended, endedHandler := probeTestConnection("ended")
		close(endedHandler.done)
		noProber := &probeTestConnector{server: "noprobe", handler: doneOnlyHandler{done: make(chan struct{})}}
		var conns []connectors.Connector
		var probed []*probeTestHandler
		conns = append(conns, ended, noProber)
		for i := range 4 {
			conn, handler := probeTestConnection(fmt.Sprintf("srv%d", i))
			conns = append(conns, conn)
			probed = append(probed, handler)
		}
		c, logger := newProbeTestClient([]string{"/var/log/a.log", "-", "/var/log/with space.log"}, conns...)

		ctx, cancel := context.WithCancel(t.Context())
		c.prober.start(ctx)
		synctest.Wait()
		cancel()
		c.prober.wait()

		wantFiles := []string{"/var/log/a.log", protocol.EncodeReadPath("/var/log/with space.log")}
		for i, handler := range probed[:probeMaxConnections] {
			requests := handler.recorded()
			if len(requests) != 1 || !reflect.DeepEqual(requests[0].Files, wantFiles) ||
				!protocol.ValidProbeID(requests[0].ID) {
				t.Fatalf("srv%d requests = %+v, want one for %q", i, requests, wantFiles)
			}
		}
		if requests := probed[probeMaxConnections].recorded(); len(requests) != 0 {
			t.Fatalf("fourth connection probed: %+v", requests)
		}
		if requests := endedHandler.recorded(); len(requests) != 0 {
			t.Fatalf("ended connection probed: %+v", requests)
		}
		snap := c.prober.catalogSnapshot()
		if !reflect.DeepEqual(snap.Servers, []string{"srv0", "srv1", "srv2"}) || snap.DefaultLogFormat != "default" {
			t.Fatalf("catalog = %+v", snap)
		}
		logger.requireSilent(t)
	})
}

// TestInteractiveProberRequests: a request (as after an applied :reload)
// runs a round with the session's current files and connections, so a
// connection replaced by a reconnect is probed and the one it replaced is
// not.
func TestInteractiveProberRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		old, oldHandler := probeTestConnection("srv")
		c, _ := newProbeTestClient([]string{"/a.log"}, old)
		ctx, cancel := context.WithCancel(t.Context())
		defer func() { cancel(); c.prober.wait() }()
		c.prober.start(ctx)
		synctest.Wait()

		replacement, newHandler := probeTestConnection("srv")
		c.replaceConnection(0, replacement)
		c.storeReloadState(c.Args, SessionSpec{Mode: omode.TailClient, Files: []string{"/b.log"}})
		c.prober.request()
		synctest.Wait()

		if requests := oldHandler.recorded(); len(requests) != 1 {
			t.Fatalf("replaced connection requests = %+v, want the first round only", requests)
		}
		requests := newHandler.recorded()
		if len(requests) != 1 || !reflect.DeepEqual(requests[0].Files, []string{"/b.log"}) {
			t.Fatalf("new connection requests = %+v, want one for /b.log", requests)
		}
		got := c.prober.catalogSnapshot().Formats["default"].Tables["STATS"]["files"]
		if !reflect.DeepEqual(got, []string{"/b.log"}) {
			t.Fatalf("catalog files = %q, want the answer of the new round only", got)
		}
	})
}

// TestInteractiveProberPeriodicRounds: a round runs every interval after
// the previous one while the output is not paused, and none while it is.
func TestInteractiveProberPeriodicRounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, handler := probeTestConnection("srv")
		c, _ := newProbeTestClient([]string{"/a.log"}, conn)
		ctx, cancel := context.WithCancel(t.Context())
		defer func() { cancel(); c.prober.wait() }()
		c.prober.start(ctx)
		synctest.Wait()
		rounds := func() int { return len(handler.recorded()) }
		if rounds() != 1 {
			t.Fatalf("rounds after start = %d, want 1", rounds())
		}

		time.Sleep(probeInterval + time.Second)
		synctest.Wait()
		if rounds() != 2 {
			t.Fatalf("rounds after one interval = %d, want 2", rounds())
		}

		c.prober.setOutputPaused(true)
		time.Sleep(3 * probeInterval)
		synctest.Wait()
		if rounds() != 2 {
			t.Fatalf("rounds while paused = %d, want 2", rounds())
		}

		c.prober.setOutputPaused(false)
		time.Sleep(probeInterval)
		synctest.Wait()
		if rounds() != 3 {
			t.Fatalf("rounds after resume = %d, want 3", rounds())
		}
	})
}

// TestInteractiveProberFailures: a probe that fails or gets no answer
// within the round timeout stores nothing and prints nothing, a session
// without files is not probed, and the prober stops at once with its
// context, also while a probe waits.
func TestInteractiveProberFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		failing, failingHandler := probeTestConnection("failing")
		failingHandler.err = errors.New("boom")
		silent, silentHandler := probeTestConnection("silent")
		silentHandler.block = true
		c, logger := newProbeTestClient([]string{"/a.log"}, failing, silent)
		ctx, cancel := context.WithCancel(t.Context())
		c.prober.start(ctx)

		time.Sleep(probeRoundTimeout + time.Second)
		synctest.Wait()
		silentHandler.mu.Lock()
		ctxErrs := slices.Clone(silentHandler.ctxErrs)
		silentHandler.mu.Unlock()
		if !reflect.DeepEqual(ctxErrs, []error{context.DeadlineExceeded}) {
			t.Fatalf("silent probe ended with %v, want the round timeout", ctxErrs)
		}
		if snap := c.prober.catalogSnapshot(); len(snap.Servers) != 0 {
			t.Fatalf("failed probes stored %+v", snap)
		}

		c.prober.request() // blocks in the silent probe until canceled
		synctest.Wait()
		cancel()
		c.prober.wait()
		if len(silentHandler.recorded()) != 2 {
			t.Fatalf("silent probes = %d, want 2", len(silentHandler.recorded()))
		}
		logger.requireSilent(t)
	})

	synctest.Test(t, func(t *testing.T) {
		conn, handler := probeTestConnection("srv")
		c, _ := newProbeTestClient([]string{"-"}, conn)
		ctx, cancel := context.WithCancel(t.Context())
		c.prober.start(ctx)
		synctest.Wait()
		cancel()
		c.prober.wait()
		if requests := handler.recorded(); len(requests) != 0 {
			t.Fatalf("stdin session probed: %+v", requests)
		}
	})
}

// TestNilInteractiveProber: a client without an interactive session has no
// prober; requests and pause changes are no-ops.
func TestNilInteractiveProber(t *testing.T) {
	var prober *interactiveProber
	prober.request()
	prober.setOutputPaused(true)
}

// TestInteractiveProberRequestsCoalesce: requests made while one is pending
// (e.g. during a round) never block and add up to one more round.
func TestInteractiveProberRequestsCoalesce(t *testing.T) {
	c, _ := newProbeTestClient([]string{"/a.log"})
	for range 3 {
		c.prober.request()
	}
	if len(c.prober.trigger) != 1 {
		t.Fatalf("pending requests = %d, want 1", len(c.prober.trigger))
	}
}

// doneOnlyHandler is a connection handler that cannot probe.
type doneOnlyHandler struct {
	handlers.Handler
	done chan struct{}
}

func (h doneOnlyHandler) Done() <-chan struct{} { return h.done }

func TestProbeFiles(t *testing.T) {
	many := make([]string, protocol.ProbeMaxFiles+3)
	for i := range many {
		many[i] = fmt.Sprintf("/f%d", i)
	}
	tests := []struct {
		name  string
		files []string
		want  []string
	}{
		{name: "none", files: nil, want: []string{}},
		{name: "stdin and blank", files: []string{"-", ""}, want: []string{}},
		{name: "encoded", files: []string{"/a b", "base64%x", "journal:x.service"},
			want: []string{protocol.EncodeReadPath("/a b"), protocol.EncodeReadPath("base64%x"), "journal:x.service"}},
		{name: "capped", files: many, want: many[:protocol.ProbeMaxFiles]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := probeFiles(tt.files); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("probeFiles(%q) = %q, want %q", tt.files, got, tt.want)
			}
		})
	}
}

// TestInteractivePauseTellsProber: pausing and resuming the output (also
// for typing) is passed on to the prober, which holds back its periodic
// rounds while paused.
func TestInteractivePauseTellsProber(t *testing.T) {
	logger := newPausingTestLogger()
	c := newPauseTestClient(logger)
	c.prober = c.newInteractiveProber()
	pause := c.newInteractivePause()

	pause.pause()
	if !c.prober.outputPaused.Load() {
		t.Fatal("pause not passed on")
	}
	pause.resume()
	if c.prober.outputPaused.Load() {
		t.Fatal("resume not passed on")
	}
	pause.pauseTyping()
	if !c.prober.outputPaused.Load() {
		t.Fatal("typing pause not passed on")
	}
	pause.resume()
	if c.prober.outputPaused.Load() {
		t.Fatal("resume after typing not passed on")
	}
}

// TestInteractiveProberDropsAnswersOfOtherFiles: after a :reload to other
// files, the catalog holds only answers to probes of the current files: a
// server that is not asked again (or whose probe fails) does not keep
// adding the fields of the files read before.
func TestInteractiveProberDropsAnswersOfOtherFiles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, _ := probeTestConnection("srv0")
		second, secondHandler := probeTestConnection("srv1")
		c, _ := newProbeTestClient([]string{"/a.log"}, first, second)
		ctx, cancel := context.WithCancel(t.Context())
		defer func() { cancel(); c.prober.wait() }()
		c.prober.start(ctx)
		synctest.Wait()
		if snap := c.prober.catalogSnapshot(); len(snap.Servers) != 2 {
			t.Fatalf("servers after the first round = %q, want both", snap.Servers)
		}

		secondHandler.err = errors.New("boom")
		c.storeReloadState(c.Args, SessionSpec{Mode: omode.TailClient, Files: []string{"/b.log"}})
		c.prober.request()
		synctest.Wait()

		snap := c.prober.catalogSnapshot()
		got := snap.Formats["default"].Tables["STATS"]["files"]
		if !reflect.DeepEqual(got, []string{"/b.log"}) || !reflect.DeepEqual(snap.Servers, []string{"srv0"}) {
			t.Fatalf("catalog after reload: servers %q, files %q, want srv0 with /b.log only", snap.Servers, got)
		}

		// Back on the first files, srv1's answer about them counts again,
		// before any round, and srv0's answer about /b.log does not.
		c.storeReloadState(c.Args, SessionSpec{Mode: omode.TailClient, Files: []string{"/a.log"}})
		if snap := c.prober.catalogSnapshot(); !reflect.DeepEqual(snap.Servers, []string{"srv1"}) {
			t.Fatalf("servers back on /a.log before a round = %q, want srv1", snap.Servers)
		}
	})
}

package clients

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	sessionspec "github.com/mimecast/dtail/internal/session"

	"golang.org/x/crypto/ssh"
)

func TestParseInteractiveCommandForGrepReload(t *testing.T) {
	current := config.Args{
		Mode:     omode.GrepClient,
		What:     "/var/log/app.log",
		RegexStr: "ERROR",
	}

	command, err := parseInteractiveCommand(current,
		`:reload --grep WARN --before 2 --after 3 --max 4 --invert --files "/tmp/other.log" --plain --quiet --timeout 7`,
		logging.NopLogger{})
	if err != nil {
		t.Fatalf("parseInteractiveCommand() error = %v", err)
	}

	if command.kind != "reload" {
		t.Fatalf("command kind = %q, want reload", command.kind)
	}
	if command.next.What != "/tmp/other.log" {
		t.Fatalf("files = %q, want /tmp/other.log", command.next.What)
	}
	if command.next.RegexStr != "WARN" {
		t.Fatalf("regex = %q, want WARN", command.next.RegexStr)
	}
	if !command.next.RegexInvert || !command.next.Plain || !command.next.Quiet {
		t.Fatalf("expected invert/plain/quiet flags to be set: %#v", command.next)
	}
	if command.next.BeforeContext != 2 || command.next.AfterContext != 3 || command.next.MaxCount != 4 {
		t.Fatalf("unexpected context values: %#v", command.next.LContext)
	}
	if command.spec.Regex != "WARN" {
		t.Fatalf("session spec regex = %q, want WARN", command.spec.Regex)
	}
}

func TestParseInteractiveCommandForMapReloadDerivesRegex(t *testing.T) {
	current := config.Args{
		Mode:     omode.MapClient,
		What:     "/var/log/app.log",
		QueryStr: "select count(status) from stats group by status",
	}

	command, err := parseInteractiveCommand(current,
		`:reload --query "select count(status) from warnings group by status" --files /tmp/new.log --plain --timeout 9`,
		logging.NopLogger{})
	if err != nil {
		t.Fatalf("parseInteractiveCommand() error = %v", err)
	}

	if command.kind != "reload" {
		t.Fatalf("command kind = %q, want reload", command.kind)
	}
	if command.next.QueryStr != "select count(status) from warnings group by status" {
		t.Fatalf("query = %q", command.next.QueryStr)
	}
	if command.spec.Regex != "\\|MAPREDUCE:WARNINGS\\|" {
		t.Fatalf("session spec regex = %q, want WARNINGS table regex", command.spec.Regex)
	}
}

func TestParseInteractiveCommandRejectsUnterminatedQuotes(t *testing.T) {
	current := config.Args{
		Mode:     omode.MapClient,
		QueryStr: "select count(status) from stats group by status",
	}

	if _, err := parseInteractiveCommand(current,
		`:reload --query "select count(status) from stats`, logging.NopLogger{}); err == nil {
		t.Fatalf("expected parseInteractiveCommand() to reject unterminated quoted input")
	}
}

func TestApplyInteractiveReloadRejectsUnsupportedConnections(t *testing.T) {
	client := &baseClient{
		mu: newBaseClientMu(),
		Args: config.Args{
			Mode:     omode.GrepClient,
			What:     "/var/log/app.log",
			RegexStr: "ERROR",
		},
		sessionSpec: SessionSpec{
			Mode:  omode.GrepClient,
			Files: []string{"/var/log/app.log"},
			Regex: "ERROR",
		},
		connections: []connectors.Connector{
			&interactiveReloadConnector{server: "srv1", supported: false},
		},
	}

	err := client.applyInteractiveReload(config.Args{
		Mode:     omode.GrepClient,
		What:     "/tmp/next.log",
		RegexStr: "WARN",
	}, SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/tmp/next.log"},
		Regex: "WARN",
	})
	if !errors.Is(err, connectors.ErrSessionUnsupported) {
		t.Fatalf("expected ErrSessionUnsupported, got %v", err)
	}
	if client.What != "/var/log/app.log" || client.sessionSpec.Regex != "ERROR" {
		t.Fatalf("client state changed on unsupported reload: args=%#v spec=%#v", client.Args, client.sessionSpec)
	}
}

func TestApplyInteractiveReloadRollsBackEarlyFailure(t *testing.T) {
	oldArgs := config.Args{
		Mode:     omode.GrepClient,
		What:     "/var/log/app.log",
		RegexStr: "ERROR",
	}
	oldSpec := SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}
	rollbackErr := errors.New("boom")

	connA := &interactiveReloadConnector{server: "srv1", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	connB := &interactiveReloadConnector{server: "srv2", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	connC := &interactiveReloadConnector{server: "srv3", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, applyErr: rollbackErr, generation: 4}
	recorder := &interactiveReloadRecorder{}

	client := &baseClient{
		mu:          newBaseClientMu(),
		Args:        oldArgs,
		sessionSpec: oldSpec,
		connections: []connectors.Connector{connA, connB, connC},
		profile: clientProfile{
			commit: recorder.commitSessionSpec,
		},
	}

	nextArgs := config.Args{
		Mode:     omode.GrepClient,
		What:     "/tmp/new.log",
		RegexStr: "WARN",
	}
	nextSpec := SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/tmp/new.log"},
		Regex: "WARN",
	}

	err := client.applyInteractiveReload(nextArgs, nextSpec)
	if err == nil || !errors.Is(err, rollbackErr) {
		t.Fatalf("expected reload error from failing connection, got %v", err)
	}

	if !reflect.DeepEqual(client.Args, oldArgs) || !reflect.DeepEqual(client.sessionSpec, oldSpec) {
		t.Fatalf("client state changed on partial failure: args=%#v spec=%#v", client.Args, client.sessionSpec)
	}
	if len(recorder.commits) != 0 {
		t.Fatalf("expected no committed shared state, got %#v", recorder.commits)
	}
	if !reflect.DeepEqual(connA.committedSpec, oldSpec) || !reflect.DeepEqual(connB.committedSpec, oldSpec) {
		t.Fatalf("expected successful connections to roll back to old spec: %#v %#v", connA.committedSpec, connB.committedSpec)
	}
	if connA.generation != 6 || connB.generation != 6 {
		t.Fatalf("expected rollback to advance generations through the remote update path, got %d and %d", connA.generation, connB.generation)
	}
	if connA.applyCount != 2 || connB.applyCount != 2 || connC.applyCount != 1 {
		t.Fatalf("unexpected apply counts during rollback: %#v %#v %#v", connA.applyCount, connB.applyCount, connC.applyCount)
	}
	if !reflect.DeepEqual(connA.liveSpec, oldSpec) || !reflect.DeepEqual(connB.liveSpec, oldSpec) {
		t.Fatalf("expected rollback to restore live specs on successful connections: %#v %#v", connA.liveSpec, connB.liveSpec)
	}
	if !reflect.DeepEqual(connC.committedSpec, oldSpec) || connC.generation != 4 {
		t.Fatalf("failed connection should remain on original committed session: %#v generation=%d", connC.committedSpec, connC.generation)
	}
}

func TestApplyInteractiveReloadRollsBackLateAckFailure(t *testing.T) {
	oldArgs := config.Args{
		Mode:     omode.GrepClient,
		What:     "/var/log/app.log",
		RegexStr: "ERROR",
	}
	oldSpec := SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}
	nextSpec := SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/tmp/new.log"},
		Regex: "WARN",
	}

	connA := &interactiveReloadConnector{server: "srv1", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	connB := &interactiveReloadConnector{server: "srv2", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	connC := &interactiveReloadConnector{
		server:                   "srv3",
		supported:                true,
		committedSpec:            oldSpec,
		liveSpec:                 oldSpec,
		generation:               4,
		applyErrAfterAdvance:     connectors.ErrSessionAckTimeout,
		applyErrAfterAdvanceSpec: nextSpec,
	}

	client := &baseClient{
		mu:          newBaseClientMu(),
		Args:        oldArgs,
		sessionSpec: oldSpec,
		connections: []connectors.Connector{connA, connB, connC},
	}

	nextArgs := config.Args{
		Mode:     omode.GrepClient,
		What:     "/tmp/new.log",
		RegexStr: "WARN",
	}
	err := client.applyInteractiveReload(nextArgs, nextSpec)
	if err == nil || !errors.Is(err, connectors.ErrSessionAckTimeout) {
		t.Fatalf("expected late ack timeout error, got %v", err)
	}

	if !reflect.DeepEqual(client.Args, oldArgs) || !reflect.DeepEqual(client.sessionSpec, oldSpec) {
		t.Fatalf("client state changed on late failure: args=%#v spec=%#v", client.Args, client.sessionSpec)
	}
	if !reflect.DeepEqual(connA.committedSpec, oldSpec) || !reflect.DeepEqual(connB.committedSpec, oldSpec) {
		t.Fatalf("expected successful connections to roll back to old spec: %#v %#v", connA.committedSpec, connB.committedSpec)
	}
	if connA.generation != 6 || connB.generation != 6 {
		t.Fatalf("expected rollback to advance generations through the remote update path, got %d and %d", connA.generation, connB.generation)
	}
	if connA.applyCount != 2 || connB.applyCount != 2 || connC.applyCount != 2 {
		t.Fatalf("unexpected apply counts during late rollback: %#v %#v %#v", connA.applyCount, connB.applyCount, connC.applyCount)
	}
	if !reflect.DeepEqual(connA.liveSpec, oldSpec) || !reflect.DeepEqual(connB.liveSpec, oldSpec) {
		t.Fatalf("expected rollback to restore live specs on successful connections: %#v %#v", connA.liveSpec, connB.liveSpec)
	}
	if !reflect.DeepEqual(connC.committedSpec, oldSpec) || !reflect.DeepEqual(connC.liveSpec, oldSpec) || connC.generation != 6 {
		t.Fatalf("late-failing connection should also roll back to original committed session: committed=%#v live=%#v generation=%d", connC.committedSpec, connC.liveSpec, connC.generation)
	}
}

func TestApplyInteractiveReloadRollsBackLateAckFailureWithRealServerConnection(t *testing.T) {
	oldArgs := config.Args{
		Mode:     omode.GrepClient,
		What:     "/var/log/app.log",
		RegexStr: "ERROR",
	}
	oldSpec := SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}
	nextSpec := SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/tmp/new.log"},
		Regex: "WARN",
	}

	handlerA := newInteractiveReloadSessionHandler(
		handlers.SessionAck{Action: "update", Generation: 5},
		handlers.SessionAck{Action: "update", Generation: 6},
	)
	handlerB := newInteractiveReloadSessionHandler(
		handlers.SessionAck{Action: "update", Generation: 5},
		handlers.SessionAck{Action: "update", Generation: 6},
	)
	handlerC := newInteractiveReloadSessionHandler()
	connA := newInteractiveReloadServerConnection(t, "srv1", handlerA, oldSpec)
	connB := newInteractiveReloadServerConnection(t, "srv2", handlerB, oldSpec)
	connC := newInteractiveReloadServerConnection(t, "srv3", handlerC, oldSpec)

	for _, conn := range []*connectors.ServerConnection{connA, connB, connC} {
		conn.RestoreCommittedSession(oldSpec, 4, true)
	}

	client := &baseClient{
		mu:          newBaseClientMu(),
		Args:        oldArgs,
		sessionSpec: oldSpec,
		connections: []connectors.Connector{connA, connB, connC},
	}

	nextArgs := config.Args{
		Mode:     omode.GrepClient,
		What:     "/tmp/new.log",
		RegexStr: "WARN",
	}
	err := client.applyInteractiveReload(nextArgs, nextSpec)
	if err == nil || !errors.Is(err, connectors.ErrSessionAckTimeout) {
		t.Fatalf("expected late ack timeout error, got %v", err)
	}

	if !reflect.DeepEqual(client.Args, oldArgs) || !reflect.DeepEqual(client.sessionSpec, oldSpec) {
		t.Fatalf("client state changed on late failure: args=%#v spec=%#v", client.Args, client.sessionSpec)
	}

	for _, conn := range []*connectors.ServerConnection{connA, connB} {
		committedSpec, generation, ok := conn.CommittedSession()
		if !ok || generation != 6 || !reflect.DeepEqual(committedSpec, oldSpec) {
			t.Fatalf("successful connection did not roll back cleanly: spec=%#v generation=%d ok=%v", committedSpec, generation, ok)
		}
	}

	if committedSpec, generation, ok := connC.CommittedSession(); !ok || generation != 4 || !reflect.DeepEqual(committedSpec, oldSpec) {
		t.Fatalf("timed-out rollback should leave failed connection at its original committed session: spec=%#v generation=%d ok=%v", committedSpec, generation, ok)
	}
	if len(handlerC.commands) != 2 {
		t.Fatalf("expected the failed connection to receive both the reload and rollback commands, got %#v", handlerC.commands)
	}
}

func TestApplyInteractiveReloadCommitsSharedState(t *testing.T) {
	oldSpec := SessionSpec{
		Mode:  omode.MapClient,
		Files: []string{"/var/log/app.log"},
		Query: "select count(status) from stats group by status",
		Regex: "\\|MAPREDUCE:STATS\\|",
	}
	connA := &interactiveReloadConnector{server: "srv1", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	connB := &interactiveReloadConnector{server: "srv2", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	recorder := &interactiveReloadRecorder{}

	client := &baseClient{
		mu: newBaseClientMu(),
		Args: config.Args{
			Mode:     omode.MapClient,
			What:     "/var/log/app.log",
			QueryStr: "select count(status) from stats group by status",
		},
		sessionSpec: SessionSpec{
			Mode:  omode.MapClient,
			Files: []string{"/var/log/app.log"},
			Query: "select count(status) from stats group by status",
			Regex: "\\|MAPREDUCE:STATS\\|",
		},
		connections: []connectors.Connector{connA, connB},
		profile: clientProfile{
			commit: recorder.commitSessionSpec,
		},
	}

	nextArgs := config.Args{
		Mode:     omode.MapClient,
		What:     "/tmp/new.log",
		QueryStr: "select count(status) from warnings group by status",
		Plain:    true,
		Timeout:  5,
	}
	nextSpec := SessionSpec{
		Mode:    omode.MapClient,
		Files:   []string{"/tmp/new.log"},
		Query:   nextArgs.QueryStr,
		Regex:   "\\|MAPREDUCE:WARNINGS\\|",
		Timeout: 5,
	}

	if err := client.applyInteractiveReload(nextArgs, nextSpec); err != nil {
		t.Fatalf("applyInteractiveReload() error = %v", err)
	}

	if client.What != "/tmp/new.log" || client.sessionSpec.Query != nextArgs.QueryStr {
		t.Fatalf("client state not committed: args=%#v spec=%#v", client.Args, client.sessionSpec)
	}
	if len(recorder.commits) != 1 {
		t.Fatalf("expected one profile commit call, got %d", len(recorder.commits))
	}
	if recorder.commits[0].generation != 5 || recorder.commits[0].spec.Query != nextArgs.QueryStr {
		t.Fatalf("unexpected commit payload: %#v", recorder.commits[0])
	}
	if connA.committedSpec.Query != nextArgs.QueryStr || connB.committedSpec.Query != nextArgs.QueryStr {
		t.Fatalf("connectors did not receive new session spec: %#v %#v", connA.committedSpec, connB.committedSpec)
	}
}

func TestApplyInteractiveReloadRollsBackProfileCommitError(t *testing.T) {
	oldArgs := config.Args{Mode: omode.MapClient, What: "/var/log/app.log"}
	oldSpec := SessionSpec{Mode: omode.MapClient, Files: []string{"/var/log/app.log"}, Query: "from STATS select count(*)"}
	nextArgs := config.Args{Mode: omode.MapClient, What: "/tmp/new.log"}
	nextSpec := SessionSpec{Mode: omode.MapClient, Files: []string{"/tmp/new.log"}, Query: "from WARNINGS select count(*)"}
	connA := &interactiveReloadConnector{server: "srv1", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	connB := &interactiveReloadConnector{server: "srv2", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	commitErr := errors.New("commit failed")
	client := &baseClient{
		mu:          newBaseClientMu(),
		Args:        oldArgs,
		sessionSpec: oldSpec,
		connections: []connectors.Connector{connA, connB},
		profile: clientProfile{
			commit: func(SessionSpec, uint64) error { return commitErr },
		},
	}

	err := client.applyInteractiveReload(nextArgs, nextSpec)
	if !errors.Is(err, commitErr) {
		t.Fatalf("applyInteractiveReload() error = %v, want commit error", err)
	}
	if !reflect.DeepEqual(client.Args, oldArgs) || !reflect.DeepEqual(client.sessionSpec, oldSpec) {
		t.Fatalf("client state changed after commit failure: args=%#v spec=%#v", client.Args, client.sessionSpec)
	}
	for _, conn := range []*interactiveReloadConnector{connA, connB} {
		if !reflect.DeepEqual(conn.committedSpec, oldSpec) || !reflect.DeepEqual(conn.liveSpec, oldSpec) {
			t.Fatalf("connection %s did not roll back: committed=%#v live=%#v", conn.server, conn.committedSpec, conn.liveSpec)
		}
		if conn.generation != 6 || conn.applyCount != 2 {
			t.Fatalf("connection %s rollback state = generation:%d applies:%d, want 6 and 2", conn.server, conn.generation, conn.applyCount)
		}
	}
}

func TestApplyInteractiveReloadRejectsMismatchedCommittedGenerations(t *testing.T) {
	oldSpec := SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}
	connA := &interactiveReloadConnector{server: "srv1", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	connB := &interactiveReloadConnector{server: "srv2", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 5}
	recorder := &interactiveReloadRecorder{}

	client := &baseClient{
		mu: newBaseClientMu(),
		Args: config.Args{
			Mode:     omode.GrepClient,
			What:     "/var/log/app.log",
			RegexStr: "ERROR",
		},
		sessionSpec: SessionSpec{
			Mode:  omode.GrepClient,
			Files: []string{"/var/log/app.log"},
			Regex: "ERROR",
		},
		connections: []connectors.Connector{connA, connB},
		profile: clientProfile{
			commit: recorder.commitSessionSpec,
		},
	}

	nextArgs := config.Args{
		Mode:     omode.GrepClient,
		What:     "/tmp/new.log",
		RegexStr: "WARN",
	}
	nextSpec := SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/tmp/new.log"},
		Regex: "WARN",
	}

	err := client.applyInteractiveReload(nextArgs, nextSpec)
	if err == nil || err.Error() != "mismatched committed generations: got 5 and 6" {
		t.Fatalf("expected mismatched generation error, got %v", err)
	}
	if client.What != "/var/log/app.log" || client.sessionSpec.Regex != "ERROR" {
		t.Fatalf("client state changed on mismatched generations: args=%#v spec=%#v", client.Args, client.sessionSpec)
	}
	if len(recorder.commits) != 0 {
		t.Fatalf("expected no committed shared state, got %#v", recorder.commits)
	}
	if connA.generation != 6 || connB.generation != 7 {
		t.Fatalf("expected rollback to replay the previous spec remotely, got %d and %d", connA.generation, connB.generation)
	}
	if connA.applyCount != 2 || connB.applyCount != 2 {
		t.Fatalf("expected rollback to reapply the previous spec on both connections, got %d and %d", connA.applyCount, connB.applyCount)
	}
	if !reflect.DeepEqual(connA.committedSpec, oldSpec) || !reflect.DeepEqual(connB.committedSpec, oldSpec) {
		t.Fatalf("expected rollback to restore committed specs, got %#v %#v", connA.committedSpec, connB.committedSpec)
	}
}

type interactiveReloadConnector struct {
	committedSpec            sessionspec.Spec
	liveSpec                 sessionspec.Spec
	applyErr                 error
	applyErrAfterAdvance     error
	applyErrAfterAdvanceSpec sessionspec.Spec
	applyCount               int
	generation               uint64
	server                   string
	supported                bool
}

type interactiveReloadSessionHandler struct {
	commands            []string
	capabilities        map[string]bool
	sessionAcks         []handlers.SessionAck
	waitForCapabilities bool
}

var _ handlers.Handler = (*interactiveReloadSessionHandler)(nil)

func newInteractiveReloadSessionHandler(acks ...handlers.SessionAck) *interactiveReloadSessionHandler {
	return &interactiveReloadSessionHandler{
		capabilities: map[string]bool{
			protocol.CapabilityQueryUpdateV1: true,
		},
		sessionAcks:         append([]handlers.SessionAck(nil), acks...),
		waitForCapabilities: true,
	}
}

func (h *interactiveReloadSessionHandler) SendMessage(command string) error {
	h.commands = append(h.commands, command)
	return nil
}

func (h *interactiveReloadSessionHandler) Capabilities() []string {
	capabilities := make([]string, 0, len(h.capabilities))
	for capability := range h.capabilities {
		capabilities = append(capabilities, capability)
	}
	return capabilities
}

func (h *interactiveReloadSessionHandler) HasCapability(name string) bool {
	return h.capabilities[name]
}

func (*interactiveReloadSessionHandler) ReportServerError(string) {}

func (*interactiveReloadSessionHandler) Server() string { return "mock" }

func (*interactiveReloadSessionHandler) Status() int { return 0 }

func (*interactiveReloadSessionHandler) Shutdown() {}

func (*interactiveReloadSessionHandler) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (h *interactiveReloadSessionHandler) WaitForCapabilities(time.Duration) bool {
	return h.waitForCapabilities
}

func (h *interactiveReloadSessionHandler) WaitForSessionAck(timeout time.Duration) (handlers.SessionAck, bool) {
	if timeout <= 0 {
		return handlers.SessionAck{}, false
	}
	if len(h.sessionAcks) == 0 {
		return handlers.SessionAck{}, false
	}

	ack := h.sessionAcks[0]
	h.sessionAcks = h.sessionAcks[1:]
	return ack, true
}

func (*interactiveReloadSessionHandler) Read(_ []byte) (int, error) {
	return 0, nil
}

func (*interactiveReloadSessionHandler) Write(p []byte) (int, error) {
	return len(p), nil
}

type interactiveReloadHostKeyCallback struct{}

func (interactiveReloadHostKeyCallback) Wrap(context.Context) ssh.HostKeyCallback {
	return ssh.InsecureIgnoreHostKey()
}

func (interactiveReloadHostKeyCallback) Untrusted(string) bool { return false }

func (interactiveReloadHostKeyCallback) PromptAddHosts(context.Context) {}

func newInteractiveReloadServerConnection(t *testing.T, server string, handler handlers.Handler, spec SessionSpec) *connectors.ServerConnection {
	t.Helper()

	conn, err := connectors.NewServerConnection(
		server,
		"user",
		nil,
		interactiveReloadHostKeyCallback{},
		handler,
		nil,
		spec,
		true,
		"",
		false,
		nil,
		logging.NopLogger{},
	)
	if err != nil {
		t.Fatalf("NewServerConnection: %v", err)
	}
	conn.RestoreCommittedSession(spec, 4, true)
	return conn
}

func (*interactiveReloadConnector) Start(context.Context, context.CancelFunc, chan struct{}, chan struct{}) {
}

func (c *interactiveReloadConnector) Server() string { return c.server }

func (*interactiveReloadConnector) Handler() handlers.Handler { return nil }

func (c *interactiveReloadConnector) SupportsQueryUpdates(time.Duration) bool { return c.supported }

func (c *interactiveReloadConnector) ApplySessionSpec(spec sessionspec.Spec, _ time.Duration) error {
	_, generation, ok := c.CommittedSession()
	if !ok {
		generation = 0
	}
	return c.ApplySessionSpecWithGeneration(spec, generation, 0)
}

func (c *interactiveReloadConnector) ApplySessionSpecWithGeneration(spec sessionspec.Spec, generation uint64, _ time.Duration) error {
	c.applyCount++
	if c.applyErrAfterAdvance != nil && reflect.DeepEqual(spec, c.applyErrAfterAdvanceSpec) {
		c.liveSpec = spec
		return c.applyErrAfterAdvance
	}
	if c.applyErr != nil {
		return c.applyErr
	}
	if generation == 0 {
		c.generation = 1
	} else {
		c.generation = generation + 1
	}
	c.liveSpec = spec
	c.committedSpec = spec
	return nil
}

func (c *interactiveReloadConnector) CommittedSession() (sessionspec.Spec, uint64, bool) {
	if c.generation == 0 {
		return sessionspec.Spec{}, 0, false
	}
	return c.committedSpec, c.generation, true
}

func (c *interactiveReloadConnector) RestoreCommittedSession(spec sessionspec.Spec, generation uint64, committed bool) {
	c.liveSpec = spec
	c.committedSpec = spec
	c.generation = generation
}

type interactiveReloadRecorder struct {
	commits []interactiveReloadCommit
}

type interactiveReloadCommit struct {
	generation uint64
	spec       SessionSpec
}

func (r *interactiveReloadRecorder) commitSessionSpec(spec SessionSpec, generation uint64) error { //nolint:unparam // The profile commit callback permits commit errors.
	r.commits = append(r.commits, interactiveReloadCommit{
		generation: generation,
		spec:       spec,
	})
	return nil
}

package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/session"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

func TestServerHandlerConstructorsReturnRecoverableErrors(t *testing.T) {
	store := authkey.New(time.Hour, 5)
	serverUser := &userserver.User{Name: "test-user"}

	tests := []struct {
		name string
		new  func() error
		want string
	}{
		{
			name: "missing config",
			new: func() error {
				_, err := NewServerHandler(context.Background(), serverUser, Dependencies{AuthKeyStore: store})
				return err
			},
			want: "server config",
		},
		{
			name: "missing server context",
			new: func() error {
				var nilContext context.Context
				_, err := NewServerHandler(nilContext, serverUser, Dependencies{AuthKeyStore: store})
				return err
			},
			want: "context",
		},
		{
			name: "missing auth store",
			new: func() error {
				_, err := NewServerHandler(context.Background(), serverUser, Dependencies{ServerConfig: &config.ServerConfig{}})
				return err
			},
			want: "auth-key store",
		},
		{
			name: "missing health user",
			new: func() error {
				_, err := NewHealthHandler(context.Background(), nil,
					config.DefaultMaxCommandFrameSize, "test-host", handlerTestLogger)
				return err
			},
			want: "user",
		},
		{
			name: "missing health context",
			new: func() error {
				var nilContext context.Context
				_, err := NewHealthHandler(nilContext, serverUser,
					config.DefaultMaxCommandFrameSize, "test-host", handlerTestLogger)
				return err
			},
			want: "context",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.new()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("constructor error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestHandlerConstructorsUseInjectedHostname(t *testing.T) {
	serverUser := &userserver.User{Name: "test-user"}
	serverHandler, err := NewServerHandler(context.Background(), serverUser, Dependencies{
		ServerConfig: &config.ServerConfig{},
		AuthKeyStore: authkey.New(time.Hour, 5),
		Hostname:     "server.example",
	})
	if err != nil {
		t.Fatalf("NewServerHandler error = %v", err)
	}
	if serverHandler.hostname != "server" {
		t.Fatalf("server hostname = %q, want short injected hostname", serverHandler.hostname)
	}

	healthHandler, err := NewHealthHandler(context.Background(), serverUser,
		config.DefaultMaxCommandFrameSize, "health.example", handlerTestLogger)
	if err != nil {
		t.Fatalf("NewHealthHandler error = %v", err)
	}
	if healthHandler.hostname != "health" {
		t.Fatalf("health hostname = %q, want short injected hostname", healthHandler.hostname)
	}
}

func TestNewServerHandlerSendsAdvertisedServerCapabilities(t *testing.T) {

	advertisedCapabilities := []string{
		protocol.CapabilityQueryUpdateV1,
		protocol.CapabilityJournalV1,
	}

	handler, err := NewServerHandler(context.Background(),
		&userserver.User{Name: "session-capability-user"},
		Dependencies{
			CatLimiter:   make(chan struct{}, 1),
			TailLimiter:  make(chan struct{}, 1),
			ServerConfig: &config.ServerConfig{AuthKeyEnabled: true},
			AuthKeyStore: authkey.New(time.Hour, 5),
			Loggers:      HandlerLoggers{Diagnostics: handlerTestLogger, Reader: handlerTestLogger},
			Capabilities: advertisedCapabilities,
			Hostname:     "test-host",
		},
	)
	if err != nil {
		t.Fatalf("NewServerHandler: %v", err)
	}

	message := readServerMessage(t, handler.serverMessages)
	if !strings.HasPrefix(message, protocol.HiddenCapabilitiesPrefix) {
		t.Fatalf("unexpected capability advertisement: %q", message)
	}
	if want := protocol.HiddenCapabilitiesPrefix + strings.Join(advertisedCapabilities, " "); message != want {
		t.Fatalf("capability advertisement = %q, want %q", message, want)
	}

	capabilities := strings.Fields(strings.TrimPrefix(message, protocol.HiddenCapabilitiesPrefix))
	if !slices.Contains(capabilities, protocol.CapabilityQueryUpdateV1) {
		t.Fatalf("expected %q capability in %q", protocol.CapabilityQueryUpdateV1, message)
	}
	if !slices.Contains(capabilities, protocol.CapabilityJournalV1) {
		t.Fatalf("expected %q capability in %q", protocol.CapabilityJournalV1, message)
	}
}

func TestServerCapabilitiesAdvertisesJournalOnlyOnLinuxWithJournalctl(t *testing.T) {
	tests := []struct {
		name                string
		goos                string
		journalctlAvailable bool
		want                []string
	}{
		{
			name:                "linux with journalctl",
			goos:                "linux",
			journalctlAvailable: true,
			want: []string{
				protocol.CapabilityQueryUpdateV1,
				protocol.CapabilityJournalV1,
			},
		},
		{
			name:                "linux without journalctl",
			goos:                "linux",
			journalctlAvailable: false,
			want:                []string{protocol.CapabilityQueryUpdateV1},
		},
		{
			name:                "non linux with journalctl",
			goos:                "freebsd",
			journalctlAvailable: true,
			want:                []string{protocol.CapabilityQueryUpdateV1},
		},
		{
			name:                "non linux without journalctl",
			goos:                "darwin",
			journalctlAvailable: false,
			want:                []string{protocol.CapabilityQueryUpdateV1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ServerCapabilities(tt.goos, tt.journalctlAvailable)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("capabilities = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleSessionCommandStartStoresSpec(t *testing.T) {
	handler := newSessionTestHandler("session-start-user")
	readServerMessage(t, handler.serverMessages)

	spec := session.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}
	payload := mustSessionPayload(t, spec)

	commandFinished := false
	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3, []string{"SESSION", "START", payload}, func() {
		commandFinished = true
	})

	if !commandFinished {
		t.Fatalf("expected commandFinished callback")
	}
	if !handler.sessionState.activeSession() {
		t.Fatalf("expected session state to become active")
	}
	if message := readServerMessage(t, handler.serverMessages); message != sessionAckStartOKPrefix+" 1" {
		t.Fatalf("unexpected session start message: %q", message)
	}
}

func TestHandleSessionCommandUpdateCancelsPreviousGenerationImmediately(t *testing.T) {
	handler, recorder := newSessionDispatchTestHandler("session-update-cancel-user")
	readServerMessage(t, handler.serverMessages)
	t.Cleanup(func() {
		if handler.sessionState.cancel != nil {
			handler.sessionState.cancel()
		}
		recorder.wg.Wait()
	})

	startPayload := mustSessionPayload(t, session.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app-a.log"},
		Regex: "ERROR",
	})
	updatePayload := mustSessionPayload(t, session.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app-b.log"},
		Regex: "WARN",
	})

	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3, []string{"SESSION", "START", startPayload}, func() {})
	if message := readServerMessage(t, handler.serverMessages); message != sessionAckStartOKPrefix+" 1" {
		t.Fatalf("unexpected session start ack: %q", message)
	}

	first := recorder.waitForStart(t)
	if !strings.Contains(first.command, "/var/log/app-a.log") {
		t.Fatalf("expected first command to target app-a.log, got %q", first.command)
	}

	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3, []string{"SESSION", "UPDATE", updatePayload}, func() {})
	if message := readServerMessage(t, handler.serverMessages); message != sessionAckUpdateOKPrefix+" 2" {
		t.Fatalf("unexpected session update ack: %q", message)
	}

	waitForContextDone(first.ctx, t)

	second := recorder.waitForStart(t)
	if !strings.Contains(second.command, "/var/log/app-b.log") {
		t.Fatalf("expected second command to target app-b.log, got %q", second.command)
	}
	select {
	case <-second.ctx.Done():
		t.Fatalf("expected replacement generation context to remain active")
	default:
	}
}

func TestHandleSessionCommandUpdateRequiresActiveSession(t *testing.T) {
	handler := newSessionTestHandler("session-update-user")
	readServerMessage(t, handler.serverMessages)

	spec := session.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}
	payload := mustSessionPayload(t, spec)

	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3, []string{"SESSION", "UPDATE", payload}, func() {})

	if message := readServerMessage(t, handler.serverMessages); message != sessionAckErrorPrefix+"session not started" {
		t.Fatalf("unexpected session update error: %q", message)
	}
}

func TestHandleSessionCommandRejectsInvalidPayload(t *testing.T) {
	handler := newSessionTestHandler("session-invalid-user")
	readServerMessage(t, handler.serverMessages)

	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3, []string{"SESSION", "START", "not-base64"}, func() {})

	if message := readServerMessage(t, handler.serverMessages); message != sessionAckErrorPrefix+"invalid session payload" {
		t.Fatalf("unexpected invalid payload message: %q", message)
	}
}

func TestHandleSessionCommandStartDispatchesQueryWorkload(t *testing.T) {
	handler, recorder := newQuerySessionDispatchTestHandler("session-query-user")
	readServerMessage(t, handler.serverMessages)

	payload := mustSessionPayload(t, session.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Query: "from STATS select count(*)",
		Regex: ".",
	})

	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3, []string{"SESSION", "START", payload}, func() {})

	if message := readServerMessage(t, handler.serverMessages); message != sessionAckStartOKPrefix+" 1" {
		t.Fatalf("unexpected query-session ack: %q", message)
	}

	first := recorder.waitForStart(t)
	if !strings.HasPrefix(first.command, "map:") {
		t.Fatalf("expected map command first, got %q", first.command)
	}
	if !strings.Contains(first.command, "from STATS select count(*)") {
		t.Fatalf("expected map command to contain query, got %q", first.command)
	}

	second := recorder.waitForStart(t)
	if !strings.HasPrefix(second.command, "tail:") {
		t.Fatalf("expected tail command second, got %q", second.command)
	}
	if !strings.Contains(second.command, "/var/log/app.log") {
		t.Fatalf("expected tail command to contain file, got %q", second.command)
	}
}

func TestHandleSessionCommandRejectsInvalidSerializedOptions(t *testing.T) {
	handler := newSessionTestHandler("session-options-user")
	readServerMessage(t, handler.serverMessages)

	payload := mustSessionPayload(t, session.Spec{
		Mode:    omode.TailClient,
		Files:   []string{"/var/log/app.log"},
		Options: "badoption",
		Regex:   "ERROR",
	})

	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3, []string{"SESSION", "START", payload}, func() {})

	if message := readServerMessage(t, handler.serverMessages); message != sessionAckErrorPrefix+"invalid session spec" {
		t.Fatalf("unexpected invalid options error: %q", message)
	}
}

func TestHandleSessionCommandRejectsInvalidQuerySession(t *testing.T) {
	handler := newSessionTestHandler("session-invalid-query-user")
	readServerMessage(t, handler.serverMessages)

	payload := mustSessionPayload(t, session.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Query: "select from",
		Regex: ".",
	})

	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3, []string{"SESSION", "START", payload}, func() {})

	if message := readServerMessage(t, handler.serverMessages); message != sessionAckErrorPrefix+"invalid session spec" {
		t.Fatalf("unexpected invalid query-session error: %q", message)
	}
}

func TestHandleAckCommandCloseConnectionConcurrentDoesNotPanic(t *testing.T) {
	handler := newSessionTestHandler("ack-close-user")

	const workers = 16
	start := make(chan struct{})
	panicCh := make(chan any, workers)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					panicCh <- recovered
				}
			}()
			<-start
			handler.handleAckCommand(3, []string{"ACK", "close", "connection"})
		}()
	}

	close(start)
	wg.Wait()
	close(panicCh)

	for recovered := range panicCh {
		t.Fatalf("unexpected panic while closing ack channel: %v", recovered)
	}

	select {
	case <-handler.ackCloseReceived:
	default:
		t.Fatalf("expected ackCloseReceived to be closed")
	}
}

func TestHandleSessionCommandUpdateClearsAggregateStateBeforeDirectRead(t *testing.T) {

	handler := newSessionTestHandler("session-query-reset-user")
	readServerMessage(t, handler.serverMessages)

	sawResetState := make(chan bool, 1)
	tailCalls := 0
	handler.commands = map[string]commandHandler{
		"map": func(_ context.Context, _ lcontext.LContext, argc int, args []string, commandFinished func()) {
			queryStr := strings.Join(args[1:], " ")
			// Output is now the only aggregate (task hv0), so install a
			// Aggregate as the real handleMapCommand does.
			aggregate, err := newHandlerTestAggregate(queryStr, "")
			if err != nil {
				t.Fatalf("new output aggregate: %v", err)
			}
			// Use the atomic setter so this test exercises the same code path
			// as the real handleMapCommand and avoids a direct field access race.
			handler.setAggregate(aggregate)
			commandFinished()
		},
		"tail": func(_ context.Context, _ lcontext.LContext, _ int, _ []string, commandFinished func()) {
			tailCalls++
			if tailCalls > 1 {
				// Use the atomic getter; direct field access would race with
				// concurrent reads in Shutdown/resetSessionAggregates.
				sawResetState <- handler.getAggregate() == nil
			}
			commandFinished()
		},
	}

	startPayload := mustSessionPayload(t, session.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app-a.log"},
		Query: "from STATS select count(*)",
		Regex: ".",
	})
	updatePayload := mustSessionPayload(t, session.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app-b.log"},
		Regex: "WARN",
	})

	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3, []string{"SESSION", "START", startPayload}, func() {})
	if message := readServerMessage(t, handler.serverMessages); message != sessionAckStartOKPrefix+" 1" {
		t.Fatalf("unexpected session start ack: %q", message)
	}
	// Use the atomic getter; direct field access would race with concurrent
	// writes in handleMapCommand on another goroutine.
	if handler.getAggregate() == nil {
		t.Fatalf("expected query session to install aggregate state")
	}

	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3, []string{"SESSION", "UPDATE", updatePayload}, func() {})
	if message := readServerMessage(t, handler.serverMessages); message != sessionAckUpdateOKPrefix+" 2" {
		t.Fatalf("unexpected session update ack: %q", message)
	}

	select {
	case ok := <-sawResetState:
		if !ok {
			t.Fatalf("expected aggregate state to be cleared before direct read dispatch")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for direct read dispatch")
	}
}

func newSessionTestHandler(userName string) *ServerHandler {
	testUser := &userserver.User{Name: userName}
	handler := &ServerHandler{
		baseHandler: newBaseHandler(context.Background(), baseHandlerConfig{
			logger:         handlerTestLogger,
			serverMessages: make(chan string, 8),
			maprMessages:   make(chan string, 4),
			user:           testUser,
		}),
		serverCfg: &config.ServerConfig{
			AuthKeyEnabled: true,
		},
	}
	handler.commands = map[string]commandHandler{
		"tail": immediateNoopCommandHandler,
		"cat":  immediateNoopCommandHandler,
		"grep": immediateNoopCommandHandler,
		"map":  immediateNoopCommandHandler,
	}
	handler.handleCommandCb = func(ctx context.Context, ltx lcontext.LContext, argc int, args []string, commandName string) {
		if command, found := handler.commands[commandName]; found {
			command(ctx, ltx, argc, args, func() {})
		}
	}
	handler.send(handler.serverMessages, protocol.HiddenCapabilitiesPrefix+protocol.CapabilityQueryUpdateV1)
	return handler
}

type recordedCommand struct {
	command string
	ctx     context.Context
}

type sessionDispatchRecorder struct {
	starts chan recordedCommand
	wg     sync.WaitGroup
}

func newSessionDispatchTestHandler(userName string) (*ServerHandler, *sessionDispatchRecorder) {
	handler := newSessionTestHandler(userName)
	recorder := &sessionDispatchRecorder{
		starts: make(chan recordedCommand, 4),
	}
	handler.commands = map[string]commandHandler{
		"tail": func(ctx context.Context, _ lcontext.LContext, argc int, args []string, commandFinished func()) {
			recorder.starts <- recordedCommand{
				command: strings.Join(args, " "),
				ctx:     ctx,
			}
			recorder.wg.Add(1)
			go func() {
				defer recorder.wg.Done()
				<-ctx.Done()
				commandFinished()
			}()
		},
	}
	return handler, recorder
}

func newQuerySessionDispatchTestHandler(userName string) (*ServerHandler, *sessionDispatchRecorder) {
	handler := newSessionTestHandler(userName)
	recorder := &sessionDispatchRecorder{
		starts: make(chan recordedCommand, 8),
	}
	handler.commands = map[string]commandHandler{
		"map": func(ctx context.Context, _ lcontext.LContext, _ int, args []string, commandFinished func()) {
			recorder.starts <- recordedCommand{
				command: strings.Join(args, " "),
				ctx:     ctx,
			}
			commandFinished()
		},
		"tail": func(ctx context.Context, _ lcontext.LContext, _ int, args []string, commandFinished func()) {
			recorder.starts <- recordedCommand{
				command: strings.Join(args, " "),
				ctx:     ctx,
			}
			commandFinished()
		},
		"cat": func(ctx context.Context, _ lcontext.LContext, _ int, args []string, commandFinished func()) {
			recorder.starts <- recordedCommand{
				command: strings.Join(args, " "),
				ctx:     ctx,
			}
			commandFinished()
		},
	}
	return handler, recorder
}

func immediateNoopCommandHandler(_ context.Context, _ lcontext.LContext, _ int, _ []string, commandFinished func()) {
	commandFinished()
}

func (r *sessionDispatchRecorder) waitForStart(t *testing.T) recordedCommand {
	t.Helper()

	select {
	case started := <-r.starts:
		return started
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for dispatched session command")
		return recordedCommand{}
	}
}

func mustSessionPayload(t *testing.T, spec session.Spec) string {
	t.Helper()

	payload, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal session spec: %v", err)
	}
	return base64.StdEncoding.EncodeToString(payload)
}

func TestParseSessionCommandWithGeneration(t *testing.T) {
	spec := session.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}

	action, generation, parsedSpec, err := parseSessionCommand([]string{"SESSION", "UPDATE", "7", mustSessionPayload(t, spec)}, 4, handlerTestLogger)
	if err != nil {
		t.Fatalf("parseSessionCommand error: %v", err)
	}
	if action != "UPDATE" {
		t.Fatalf("unexpected action: %s", action)
	}
	if generation != 7 {
		t.Fatalf("unexpected generation: %d", generation)
	}
	if parsedSpec.Mode != spec.Mode {
		t.Fatalf("unexpected parsed mode: %v", parsedSpec.Mode)
	}
}

func TestSessionStateStoreUpdateAutoIncrementsGeneration(t *testing.T) {
	handler := newSessionTestHandler("session-generation-user")
	readServerMessage(t, handler.serverMessages)

	startPayload := mustSessionPayload(t, session.Spec{Mode: omode.TailClient, Regex: "ERROR"})
	updatePayload := mustSessionPayload(t, session.Spec{Mode: omode.TailClient, Regex: "WARN"})

	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3, []string{"SESSION", "START", startPayload}, func() {})
	if message := readServerMessage(t, handler.serverMessages); message != sessionAckStartOKPrefix+" 1" {
		t.Fatalf("unexpected session start ack: %q", message)
	}

	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3, []string{"SESSION", "UPDATE", updatePayload}, func() {})
	if message := readServerMessage(t, handler.serverMessages); message != sessionAckUpdateOKPrefix+" 2" {
		t.Fatalf("unexpected session update ack: %q", message)
	}
}

func waitForContextDone(ctx context.Context, t *testing.T) {
	t.Helper()

	select {
	case <-ctx.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for context cancellation")
	}
}

package connectors

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	sessionHandlers "github.com/mimecast/dtail/internal/handlers"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/omode"
	sessionspec "github.com/mimecast/dtail/internal/session"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

func TestServerlessStartReturnsAfterCancellationAndDrainsServerOutput(t *testing.T) {

	clientHandler := newServerlessLifecycleClient()
	serverHandler := newServerlessLifecycleServer([]byte("final output"))
	connector := NewServerless(
		"test-user",
		clientHandler,
		nil,
		sessionspec.Spec{},
		false,
		serverlessLifecycleFactory{handler: serverHandler},
		logging.NopLogger{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		connector.Start(ctx, cancel, nil, nil)
	}()

	waitForSignal(t, clientHandler.readStarted, "client command read to start")
	waitForSignal(t, serverHandler.readStarted, "server output read to start")
	cancel()
	waitForSignal(t, startDone, "serverless connector to return after cancellation")
	waitForSignal(t, clientHandler.readFinished, "client command read to finish")

	if got, want := clientHandler.output(), "final output"; got != want {
		t.Fatalf("client output = %q, want %q", got, want)
	}
	if clientHandler.shutdownBeforeOutput() {
		t.Fatal("client handler shut down before the final server output was drained")
	}
	if got := clientHandler.shutdownCalls(); got != 1 {
		t.Fatalf("client handler Shutdown calls = %d, want 1", got)
	}
	if got := serverHandler.shutdownCalls(); got != 1 {
		t.Fatalf("server handler Shutdown calls = %d, want 1", got)
	}
	if got := serverHandler.gracefulShutdownCalls(); got != 1 {
		t.Fatalf("server handler GracefulShutdown calls = %d, want 1", got)
	}
}

func TestServerlessStartAccountsForConnectionLifetime(t *testing.T) {
	clientHandler := newBlockingDispatchServerlessClient()
	serverHandler := newServerlessLifecycleServer(nil)
	connector := NewServerless(
		"test-user",
		clientHandler,
		[]string{"health"},
		sessionspec.Spec{},
		false,
		serverlessLifecycleFactory{handler: serverHandler},
		logging.NopLogger{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(clientHandler.permitDispatch)
	throttleCh := make(chan struct{}, 1)
	statsCh := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		connector.Start(ctx, cancel, throttleCh, statsCh)
	}()

	waitForSignal(t, clientHandler.dispatchStarted, "initial command dispatch")
	if got := len(statsCh); got != 1 {
		t.Fatalf("connected count during dispatch = %d, want 1", got)
	}
	if got := len(throttleCh); got != 1 {
		t.Fatalf("throttle count during dispatch = %d, want 1", got)
	}

	clientHandler.permitDispatch()
	select {
	case throttleCh <- struct{}{}:
		// The send completes only after Start releases its establishment slot.
	case <-time.After(time.Second):
		t.Fatal("serverless connector did not release its throttle slot after command dispatch")
	}
	<-throttleCh
	if got := len(statsCh); got != 1 {
		t.Fatalf("connected count after establishment = %d, want 1", got)
	}

	cancel()
	waitForSignal(t, done, "serverless connector teardown")
	if got := len(statsCh); got != 0 {
		t.Fatalf("connected count after teardown = %d, want 0", got)
	}
	if got := len(throttleCh); got != 0 {
		t.Fatalf("throttle count after teardown = %d, want 0", got)
	}
}

func TestServerlessCancellationDrainsRealHandlerFinalAggregate(t *testing.T) {
	const statsLine = "INFO|1002-071143|1|stats.go:56|8|15|7|0.21|471h0m21s|" +
		"MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1"
	queryText := "from STATS select count($time),$time group by $time interval 3600"
	query, err := mapr.NewQuery(queryText, logging.NopLogger{})
	if err != nil {
		t.Fatalf("create client query: %v", err)
	}
	state := maprclient.NewSessionState(query, logging.NopLogger{})
	clientHandler := handlers.NewMaprHandler("local(serverless)", state, clientlog.NopLogger{})

	path := t.TempDir() + "/stats.log"
	if writeErr := os.WriteFile(path, nil, 0o600); writeErr != nil {
		t.Fatalf("create tail input: %v", writeErr)
	}
	readerLogger := newServerlessReaderSignalLogger()
	serverCfg := config.NewDefaultServerConfigForTest()
	serverCfg.MaxLineLength = len(statsLine) + 1
	serverCfg.ReadRetryIntervalMs = 10
	factory := realServerlessMapFactory{
		serverCfg:    serverCfg,
		readerLogger: readerLogger,
	}
	spec := sessionspec.Spec{
		Mode:    omode.TailClient,
		Files:   []string{path},
		Options: "plain=true:serverless=true",
		Query:   queryText,
		Regex:   "MAPREDUCE:STATS",
	}
	commands, err := spec.Commands()
	if err != nil {
		t.Fatalf("build serverless commands: %v", err)
	}
	connector := NewServerless(
		config.ContinuousUser,
		clientHandler,
		commands,
		spec,
		false,
		factory,
		logging.NopLogger{},
	)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		connector.Start(ctx, cancel, nil, nil)
	}()
	readerLogger.waitForOpen(t)

	file, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if openErr != nil {
		cancel()
		t.Fatalf("open tail input: %v", openErr)
	}
	for range 3 {
		if _, appendErr := file.WriteString(statsLine + "\n"); appendErr != nil {
			_ = file.Close()
			cancel()
			t.Fatalf("append stats input: %v", appendErr)
		}
	}
	if _, appendErr := file.WriteString(strings.Repeat("x", 128*1024) + "\n"); appendErr != nil {
		_ = file.Close()
		cancel()
		t.Fatalf("append processing sentinel: %v", appendErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		cancel()
		t.Fatalf("close tail input: %v", closeErr)
	}
	readerLogger.waitForLongLine(t)

	// Cancel the client work context while the real tail command is active.
	// The connector must first let GracefulShutdownContext claim finalization;
	// otherwise the handler's connection context aborts the aggregate and loses
	// these three samples.
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serverless connector did not finish graceful cancellation")
	}

	snapshot := state.Snapshot()
	result, rows, renderErr := snapshot.GlobalGroup.Result(snapshot.Query, 10, nil)
	if renderErr != nil {
		t.Fatalf("render final aggregate: %v", renderErr)
	}
	resultLines := strings.Split(strings.TrimSpace(result), "\n")
	resultFields := strings.Fields(resultLines[len(resultLines)-1])
	if rows != 1 || len(resultFields) == 0 || resultFields[0] != "3" {
		t.Fatalf("final aggregate rows=%d result=%q, want three drained samples", rows, result)
	}
}

func TestServerlessOutputFailureUsesAbruptShutdownAndReturnsError(t *testing.T) {

	wantErr := errors.New("client output failed")
	clientHandler := &failingServerlessClient{
		serverlessLifecycleClient: newServerlessLifecycleClient(),
		err:                       wantErr,
	}
	serverHandler := newEagerServerlessLifecycleServer([]byte("trigger output failure"))
	connector := NewServerless(
		"test-user",
		clientHandler,
		nil,
		sessionspec.Spec{},
		false,
		serverlessLifecycleFactory{handler: serverHandler},
		logging.NopLogger{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- connector.handle(ctx, cancel)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, wantErr) {
			t.Fatalf("serverless handle error = %v, want %v", err, wantErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serverless teardown blocked after its output consumer failed")
	}
	if got := serverHandler.gracefulShutdownCalls(); got != 0 {
		t.Fatalf("GracefulShutdown calls = %d, want abrupt shutdown", got)
	}
	if got := serverHandler.shutdownCalls(); got != 1 {
		t.Fatalf("Shutdown calls = %d, want 1", got)
	}
}

func TestServerlessStartReportsHandlerFactoryFailure(t *testing.T) {

	clientHandler := newServerlessLifecycleClient()
	connector := NewServerless(
		"test-user",
		clientHandler,
		nil,
		sessionspec.Spec{},
		false,
		serverlessErrorFactory{err: errors.New("permission setup failed")},
		logging.NopLogger{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connector.Start(ctx, cancel, nil, nil)
	if got := clientHandler.Status(); got != 1 {
		t.Fatalf("client handler status = %d, want 1 after factory failure", got)
	}
}

func TestServerlessAttachesOutputReaderBeforeCommandDispatch(t *testing.T) {
	clientHandler := newDispatchingServerlessClient()
	serverHandler := newServerlessLifecycleServer(nil)
	connector := NewServerless(
		"test-user",
		clientHandler,
		[]string{"health"},
		sessionspec.Spec{},
		false,
		serverlessLifecycleFactory{handler: serverHandler},
		logging.NopLogger{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- connector.handle(ctx, cancel)
	}()

	waitForSignal(t, serverHandler.writeCalled, "initial command dispatch")
	if attached, writeBeforeAttach := serverHandler.attachmentState(); !attached || writeBeforeAttach {
		t.Fatalf("reader attachment state = (attached=%v, writeBeforeAttach=%v), want (true, false)",
			attached, writeBeforeAttach)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serverless handle error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serverless connector did not stop after attachment-order check")
	}
}

type serverlessLifecycleFactory struct {
	handler sessionHandlers.Handler
}

type realServerlessMapFactory struct {
	serverCfg    *config.ServerConfig
	readerLogger logging.Logger
}

func (f realServerlessMapFactory) NewServerlessHandler(ctx context.Context, _ string) (sessionHandlers.Handler, error) {
	return sessionHandlers.NewServerHandler(ctx, &userserver.User{Name: config.ContinuousUser}, sessionHandlers.Dependencies{
		ServerConfig: f.serverCfg,
		CatLimiter:   make(chan struct{}, f.serverCfg.MaxConcurrentCats),
		TailLimiter:  make(chan struct{}, f.serverCfg.MaxConcurrentTails),
		AuthKeyStore: authkey.New(
			time.Duration(f.serverCfg.AuthKeyTTLSeconds)*time.Second,
			f.serverCfg.AuthKeyMaxPerUser,
		),
		Loggers: sessionHandlers.HandlerLoggers{
			Diagnostics: logging.NopLogger{},
			Reader:      f.readerLogger,
		},
	})
}

type serverlessReaderSignalLogger struct {
	logging.NopLogger
	openOnce     sync.Once
	longLineOnce sync.Once
	opened       chan struct{}
	longLine     chan struct{}
}

func newServerlessReaderSignalLogger() *serverlessReaderSignalLogger {
	return &serverlessReaderSignalLogger{
		opened:   make(chan struct{}),
		longLine: make(chan struct{}),
	}
}

func (l *serverlessReaderSignalLogger) Trace(args ...any) string {
	for _, arg := range args {
		if message, ok := arg.(string); ok && message == "Opened file reader" {
			l.openOnce.Do(func() { close(l.opened) })
		}
	}
	return ""
}

func (l *serverlessReaderSignalLogger) Warn(args ...any) string {
	for _, arg := range args {
		if message, ok := arg.(string); ok && message == "Long log line, splitting into multiple lines" {
			l.longLineOnce.Do(func() { close(l.longLine) })
		}
	}
	return fmt.Sprint(args...)
}

func (l *serverlessReaderSignalLogger) waitForOpen(t *testing.T) {
	t.Helper()
	select {
	case <-l.opened:
	case <-time.After(5 * time.Second):
		t.Fatal("real serverless tail reader did not open")
	}
}

func (l *serverlessReaderSignalLogger) waitForLongLine(t *testing.T) {
	t.Helper()
	select {
	case <-l.longLine:
	case <-time.After(5 * time.Second):
		t.Fatal("real serverless tail reader did not consume processing sentinel")
	}
}

type serverlessErrorFactory struct{ err error }

func (f serverlessErrorFactory) NewServerlessHandler(context.Context, string) (sessionHandlers.Handler, error) {
	return nil, f.err
}

func (f serverlessLifecycleFactory) NewServerlessHandler(context.Context, string) (sessionHandlers.Handler, error) {
	return f.handler, nil
}

type serverlessLifecycleClient struct {
	done             chan struct{}
	doneOnce         sync.Once
	readStarted      chan struct{}
	readStartedOnce  sync.Once
	readFinished     chan struct{}
	readFinishedOnce sync.Once
	mu               sync.Mutex
	received         []byte
	shutdownCount    int
	shutdownWasEarly bool
	status           int
}

func newServerlessLifecycleClient() *serverlessLifecycleClient {
	return &serverlessLifecycleClient{
		done:         make(chan struct{}),
		readStarted:  make(chan struct{}),
		readFinished: make(chan struct{}),
	}
}

var _ handlers.Handler = (*serverlessLifecycleClient)(nil)

func (*serverlessLifecycleClient) Capabilities() []string    { return nil }
func (*serverlessLifecycleClient) HasCapability(string) bool { return false }
func (h *serverlessLifecycleClient) ReportServerError(string) {
	h.mu.Lock()
	h.status = 1
	h.mu.Unlock()
}
func (*serverlessLifecycleClient) SendMessage(string) error { return nil }
func (*serverlessLifecycleClient) Server() string           { return "lifecycle" }
func (h *serverlessLifecycleClient) Status() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}
func (h *serverlessLifecycleClient) Done() <-chan struct{}                { return h.done }
func (*serverlessLifecycleClient) WaitForCapabilities(time.Duration) bool { return false }
func (*serverlessLifecycleClient) WaitForSessionAck(time.Duration) (handlers.SessionAck, bool) {
	return handlers.SessionAck{}, false
}

func (h *serverlessLifecycleClient) Read([]byte) (int, error) {
	h.readStartedOnce.Do(func() { close(h.readStarted) })
	<-h.done
	h.readFinishedOnce.Do(func() { close(h.readFinished) })
	return 0, io.EOF
}

func (h *serverlessLifecycleClient) Write(p []byte) (int, error) {
	h.mu.Lock()
	h.received = append(h.received, p...)
	h.mu.Unlock()
	return len(p), nil
}

func (h *serverlessLifecycleClient) Shutdown() {
	h.mu.Lock()
	h.shutdownCount++
	h.shutdownWasEarly = len(h.received) == 0
	h.mu.Unlock()
	h.doneOnce.Do(func() { close(h.done) })
}

func (h *serverlessLifecycleClient) output() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return string(h.received)
}

func (h *serverlessLifecycleClient) shutdownCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.shutdownCount
}

func (h *serverlessLifecycleClient) shutdownBeforeOutput() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.shutdownWasEarly
}

type serverlessLifecycleServer struct {
	payload              []byte
	shutdown             chan struct{}
	shutdownOnce         sync.Once
	readStarted          chan struct{}
	readStartedOnce      sync.Once
	writeCalled          chan struct{}
	writeCalledOnce      sync.Once
	mu                   sync.Mutex
	shutdownCount        int
	gracefulCount        int
	outputReaderAttached bool
	writeBeforeAttach    bool
	eager                bool
}

func newServerlessLifecycleServer(payload []byte) *serverlessLifecycleServer {
	return &serverlessLifecycleServer{
		payload:     payload,
		shutdown:    make(chan struct{}),
		readStarted: make(chan struct{}),
		writeCalled: make(chan struct{}),
	}
}

func newEagerServerlessLifecycleServer(payload []byte) *serverlessLifecycleServer {
	h := newServerlessLifecycleServer(payload)
	h.eager = true
	return h
}

var _ sessionHandlers.Handler = (*serverlessLifecycleServer)(nil)

func (h *serverlessLifecycleServer) Read(p []byte) (int, error) {
	h.readStartedOnce.Do(func() { close(h.readStarted) })
	if !h.eager || len(h.payload) == 0 {
		<-h.shutdown
	}
	if len(h.payload) == 0 {
		return 0, io.EOF
	}
	n := copy(p, h.payload)
	h.payload = h.payload[n:]
	return n, nil
}

func (h *serverlessLifecycleServer) AttachOutputReader() {
	h.mu.Lock()
	h.outputReaderAttached = true
	h.mu.Unlock()
}

func (h *serverlessLifecycleServer) Write(p []byte) (int, error) {
	h.mu.Lock()
	if !h.outputReaderAttached {
		h.writeBeforeAttach = true
	}
	h.mu.Unlock()
	h.writeCalledOnce.Do(func() { close(h.writeCalled) })
	return len(p), nil
}

func (h *serverlessLifecycleServer) Shutdown() {
	h.mu.Lock()
	h.shutdownCount++
	h.mu.Unlock()
	h.shutdownOnce.Do(func() { close(h.shutdown) })
}

func (h *serverlessLifecycleServer) GracefulShutdown() {
	h.mu.Lock()
	h.gracefulCount++
	h.mu.Unlock()
	h.Shutdown()
}

func (h *serverlessLifecycleServer) GracefulShutdownContext(context.Context) {
	h.GracefulShutdown()
}

func (h *serverlessLifecycleServer) Done() <-chan struct{} { return h.shutdown }

func (h *serverlessLifecycleServer) shutdownCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.shutdownCount
}

func (h *serverlessLifecycleServer) gracefulShutdownCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gracefulCount
}

func (h *serverlessLifecycleServer) attachmentState() (bool, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.outputReaderAttached, h.writeBeforeAttach
}

type dispatchingServerlessClient struct {
	*serverlessLifecycleClient
	commands chan []byte
}

type blockingDispatchServerlessClient struct {
	*dispatchingServerlessClient
	dispatchStarted chan struct{}
	dispatchOnce    sync.Once
	allowDispatch   chan struct{}
	allowOnce       sync.Once
}

func newBlockingDispatchServerlessClient() *blockingDispatchServerlessClient {
	return &blockingDispatchServerlessClient{
		dispatchingServerlessClient: newDispatchingServerlessClient(),
		dispatchStarted:             make(chan struct{}),
		allowDispatch:               make(chan struct{}),
	}
}

func (h *blockingDispatchServerlessClient) SendMessage(message string) error {
	h.dispatchOnce.Do(func() { close(h.dispatchStarted) })
	<-h.allowDispatch
	return h.dispatchingServerlessClient.SendMessage(message)
}

func (h *blockingDispatchServerlessClient) permitDispatch() {
	h.allowOnce.Do(func() { close(h.allowDispatch) })
}

func newDispatchingServerlessClient() *dispatchingServerlessClient {
	return &dispatchingServerlessClient{
		serverlessLifecycleClient: newServerlessLifecycleClient(),
		commands:                  make(chan []byte, 1),
	}
}

func (h *dispatchingServerlessClient) SendMessage(message string) error {
	h.commands <- []byte(message)
	return nil
}

func (h *dispatchingServerlessClient) Read(p []byte) (int, error) {
	select {
	case command := <-h.commands:
		return copy(p, command), nil
	case <-h.done:
		return 0, io.EOF
	}
}

type failingServerlessClient struct {
	*serverlessLifecycleClient
	err error
}

func (h *failingServerlessClient) Write([]byte) (int, error) {
	return 0, h.err
}

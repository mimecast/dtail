package handlers

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/lcontext"
	mapaggregate "github.com/mimecast/dtail/internal/mapr/aggregate"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
)

type pendingRegistrationTestServer struct {
	*globCapTestServer
	aggregate          *mapaggregate.Aggregate
	inputFinishedCalls atomic.Int32
}

func newPendingRegistrationTestServer() *pendingRegistrationTestServer {
	return &pendingRegistrationTestServer{
		globCapTestServer: newGlobCapTestServer(1000),
		aggregate:         &mapaggregate.Aggregate{},
	}
}

func (s *pendingRegistrationTestServer) Aggregate() *mapaggregate.Aggregate {
	return s.aggregate
}

func (s *pendingRegistrationTestServer) CompletePendingFile() (int32, int32) {
	remaining := atomic.AddInt32(&s.pendingFiles, -1)
	// Model the read command itself remaining active. This keeps the test
	// focused on input completion instead of invoking idle session teardown.
	return remaining, 1
}

func (s *pendingRegistrationTestServer) PendingAndActive() (int32, int32) {
	return atomic.LoadInt32(&s.pendingFiles), 1
}

func (s *pendingRegistrationTestServer) coordinateAggregateInputCompletion(
	aggregate *mapaggregate.Aggregate,
) bool {
	if aggregate != s.aggregate {
		return false
	}
	s.inputFinishedCalls.Add(1)
	return true
}

func (s *pendingRegistrationTestServer) readCommandDependencies() readCommandDependencies {
	dependencies := s.globCapTestServer.readCommandDependencies()
	dependencies.server = s
	dependencies.lifecycle = s
	dependencies.aggregates = s
	return dependencies
}

var _ readCommandServer = (*pendingRegistrationTestServer)(nil)
var _ aggregateInputBatchCoordinator = (*pendingRegistrationTestServer)(nil)
var _ readCommandDependencyProvider = (*pendingRegistrationTestServer)(nil)

func newReservedTestReadCommand(server readCommandDependencyProvider, mode omode.Mode) *readCommand {
	reservation := newPendingInputReservation(context.Background(), server, mode)
	command := newReadCommandWithAggregate(server, mode, reservation.aggregate)
	command.adoptPendingInputReservation(reservation)
	return command
}

func TestDispatchCommandReservesReadInputBeforeHandlerExecution(t *testing.T) {
	const commandCount = 64

	handler := newMapTestHandler(t)
	handler.sessionState.mu.Lock()
	handler.sessionState.active = true
	handler.sessionState.mu.Unlock()

	entered := make(chan struct{}, commandCount)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	handler.commands["cat"] = func(_ context.Context, _ lcontext.LContext, _ int,
		_ []string, commandFinished func()) {

		entered <- struct{}{}
		<-release
		commandFinished()
	}

	errs := make(chan error, commandCount)
	for range commandCount {
		go func() {
			errs <- handler.dispatchCommand(context.Background(),
				[]string{"cat", "test.log", "."}, 3)
		}()
	}
	for range commandCount {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for dispatched read handlers")
		}
	}

	if pending, _ := handler.PendingAndActive(); pending != commandCount {
		t.Fatalf("pending while dispatched handlers are blocked = %d, want %d", pending, commandCount)
	}

	releaseAll()
	for range commandCount {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("dispatch command: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for read dispatch cleanup")
		}
	}
	if pending, active := handler.PendingAndActive(); pending != 0 || active != 0 {
		t.Fatalf("handler counters after dispatch cleanup = pending %d, active %d; want zero", pending, active)
	}
}

func TestDispatchedReadCommandsRegisterAllPendingFilesBeforeCompletion(t *testing.T) {
	const commandCount = 128

	server := newPendingRegistrationTestServer()
	commands := make([]*readCommand, commandCount)
	start := make(chan struct{})
	var dispatchWg sync.WaitGroup
	dispatchWg.Add(commandCount)
	for i := range commands {
		go func(index int) {
			defer dispatchWg.Done()
			<-start
			commands[index] = newReservedTestReadCommand(server, omode.CatClient)
		}(i)
	}
	close(start)
	dispatchWg.Wait()

	if pending, _ := server.PendingAndActive(); pending != commandCount {
		t.Fatalf("pending after synchronous dispatch = %d, want %d", pending, commandCount)
	}

	// Resolve every command to a different number of files concurrently. Each
	// reservation is expanded before any completion goroutine starts, so there
	// can be no transient zero between dispatch and concrete file registration.
	totalFiles := 0
	var resolveWg sync.WaitGroup
	resolveWg.Add(commandCount)
	for i, command := range commands {
		fileCount := i%7 + 1
		totalFiles += fileCount
		go func(command *readCommand, fileCount int) {
			defer resolveWg.Done()
			command.registerPendingFiles(fileCount)
		}(command, fileCount)
	}
	resolveWg.Wait()

	if pending, _ := server.PendingAndActive(); pending != int32(totalFiles) {
		t.Fatalf("pending after reservation transfer = %d, want %d", pending, totalFiles)
	}

	var completeWg sync.WaitGroup
	completeWg.Add(totalFiles)
	for i, command := range commands {
		fileCount := i%7 + 1
		for range fileCount {
			go func(command *readCommand) {
				defer completeWg.Done()
				command.shutdownCoordinator.onFileProcessed(context.Background(), "test.log")
			}(command)
		}
	}
	completeWg.Wait()
	// Start defers this release in production. Once registration has been
	// transferred to concrete files it must be harmless and must not decrement
	// the shared counter a second time.
	for _, command := range commands {
		command.releasePendingInputReservation(context.Background())
	}

	if pending, _ := server.PendingAndActive(); pending != 0 {
		t.Fatalf("pending after all completions = %d, want 0", pending)
	}
	if calls := server.inputFinishedCalls.Load(); calls != 1 {
		t.Fatalf("aggregate input completion calls = %d, want exactly 1", calls)
	}
}

func TestDispatchedReadCommandReleasesReservationOnEarlyReturn(t *testing.T) {
	server := newPendingRegistrationTestServer()
	command := newReservedTestReadCommand(server, omode.GrepClient)

	if pending, _ := server.PendingAndActive(); pending != 1 {
		t.Fatalf("pending before command start = %d, want 1", pending)
	}
	command.Start(context.Background(), lcontext.LContext{}, 0, nil, 1)
	command.releasePendingInputReservation(context.Background())

	if pending, _ := server.PendingAndActive(); pending != 0 {
		t.Fatalf("pending after parse failure = %d, want 0", pending)
	}
	if calls := server.inputFinishedCalls.Load(); calls != 1 {
		t.Fatalf("aggregate input completion calls = %d, want exactly 1", calls)
	}
}

func TestUnclaimedReservationCompletesOlderAggregateAtFinalZero(t *testing.T) {
	server := newPendingRegistrationTestServer()
	olderRead := newReservedTestReadCommand(server, omode.CatClient)
	unclaimed := newPendingInputReservation(context.Background(), server, omode.CatClient)
	olderRead.registerPendingFiles(1)

	olderRead.shutdownCoordinator.onFileProcessed(context.Background(), "older.log")
	if pending, _ := server.PendingAndActive(); pending != 1 {
		t.Fatalf("pending after older read = %d, want unclaimed reservation", pending)
	}
	if calls := server.inputFinishedCalls.Load(); calls != 0 {
		t.Fatalf("aggregate finished before final reservation release %d times", calls)
	}

	unclaimed.releaseIfUnclaimed()
	unclaimed.releaseIfUnclaimed()
	if pending, _ := server.PendingAndActive(); pending != 0 {
		t.Fatalf("pending after unclaimed cleanup = %d, want 0", pending)
	}
	if calls := server.inputFinishedCalls.Load(); calls != 1 {
		t.Fatalf("aggregate input completion calls = %d, want exactly 1", calls)
	}
}

func TestRejectedReadDispatchReleasesReservation(t *testing.T) {
	handler := newMapTestHandler(t)
	handler.stopCommandAdmission()

	if err := handler.dispatchCommand(context.Background(),
		[]string{"cat", "test.log", "."}, 3); err != nil {
		t.Fatalf("dispatch rejected command: %v", err)
	}
	if pending, active := handler.PendingAndActive(); pending != 0 || active != 0 {
		t.Fatalf("rejected dispatch leaked counters: pending %d, active %d", pending, active)
	}
}

func TestInvalidReadOptionsReleaseDispatchReservation(t *testing.T) {
	handler := newMapTestHandler(t)

	err := handler.dispatchCommand(context.Background(),
		[]string{"cat:invalid-option", "test.log", "."}, 3)
	if err == nil {
		t.Fatal("dispatch with invalid read options returned nil error")
	}
	if pending, active := handler.PendingAndActive(); pending != 0 || active != 0 {
		t.Fatalf("invalid dispatch leaked counters: pending %d, active %d", pending, active)
	}
}

func TestUppercaseReadCommandDoesNotSuppressIdleShutdown(t *testing.T) {
	handler := newMapTestHandler(t)
	readServerMessage(t, handler.serverMessages) // Initial capability advertisement.

	dispatchDone := make(chan error, 1)
	go func() {
		dispatchDone <- handler.dispatchCommand(context.Background(),
			[]string{"CAT", "test.log", "."}, 3)
	}()
	defer handler.handleAckCommand(3, []string{".ack", "close", "connection"})

	_ = readServerMessage(t, handler.serverMessages) // Unknown-command diagnostic.
	if message, want := readServerMessage(t, handler.serverMessages),
		protocol.HiddenCommandFailedPrefix+commandFailureUnknown+"\n"; message != want {
		t.Fatalf("message after the diagnostic = %q, want the failed command %q", message, want)
	}
	if message := readServerMessage(t, handler.serverMessages); message != ".syn close connection" {
		t.Fatalf("shutdown message = %q, want close handshake", message)
	}
	handler.handleAckCommand(3, []string{".ack", "close", "connection"})

	select {
	case err := <-dispatchDone:
		if err != nil {
			t.Fatalf("dispatch uppercase command: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("uppercase read command kept dispatch blocked after close acknowledgement")
	}
	select {
	case <-handler.Done():
	case <-time.After(time.Second):
		t.Fatal("uppercase read command left an idle session open")
	}
	if pending, active := handler.PendingAndActive(); pending != 0 || active != 0 {
		t.Fatalf("uppercase dispatch leaked counters: pending %d, active %d", pending, active)
	}
}

func TestAdmittedUnclaimedReadReservationRunsIdleShutdown(t *testing.T) {
	handler := newMapTestHandler(t)
	readServerMessage(t, handler.serverMessages) // Initial capability advertisement.
	handler.commands["cat"] = func(_ context.Context, _ lcontext.LContext, _ int,
		_ []string, commandFinished func()) {

		commandFinished()
	}

	dispatchDone := make(chan error, 1)
	go func() {
		dispatchDone <- handler.dispatchCommand(context.Background(),
			[]string{"cat", "test.log", "."}, 3)
	}()
	defer handler.handleAckCommand(3, []string{".ack", "close", "connection"})

	if message := readServerMessage(t, handler.serverMessages); message != ".syn close connection" {
		t.Fatalf("shutdown message = %q, want close handshake", message)
	}
	handler.handleAckCommand(3, []string{".ack", "close", "connection"})

	select {
	case err := <-dispatchDone:
		if err != nil {
			t.Fatalf("dispatch unclaimed read command: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unclaimed reservation kept dispatch blocked after close acknowledgement")
	}
	select {
	case <-handler.Done():
	case <-time.After(time.Second):
		t.Fatal("unclaimed reservation left an idle session open")
	}
	if pending, active := handler.PendingAndActive(); pending != 0 || active != 0 {
		t.Fatalf("unclaimed dispatch leaked counters: pending %d, active %d", pending, active)
	}
}

func TestDispatchedReadCommandReleasesReservationWhenRetryIsCanceled(t *testing.T) {
	server := newPendingRegistrationTestServer()
	command := newReservedTestReadCommand(server, omode.CatClient)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	command.Start(ctx, lcontext.LContext{}, 3, []string{"cat", "missing-*.log", "."}, 10)

	if pending, _ := server.PendingAndActive(); pending != 0 {
		t.Fatalf("pending after canceled glob retry = %d, want 0", pending)
	}
	if calls := server.inputFinishedCalls.Load(); calls != 1 {
		t.Fatalf("aggregate input completion calls = %d, want exactly 1", calls)
	}
}

func TestDispatchedTailReleasesReservationWithoutFinishingAggregate(t *testing.T) {
	server := newPendingRegistrationTestServer()
	command := newReservedTestReadCommand(server, omode.TailClient)
	command.releasePendingInputReservation(context.Background())

	if pending, _ := server.PendingAndActive(); pending != 0 {
		t.Fatalf("pending after tail cancellation = %d, want 0", pending)
	}
	if calls := server.inputFinishedCalls.Load(); calls != 0 {
		t.Fatalf("tail finished aggregate input %d times, want 0", calls)
	}
}

type markerlessInputTestServer struct {
	*globCapTestServer
	aggregate *mapaggregate.Aggregate
}

func (s *markerlessInputTestServer) Aggregate() *mapaggregate.Aggregate {
	return s.aggregate
}

func (s *markerlessInputTestServer) readCommandDependencies() readCommandDependencies {
	dependencies := s.globCapTestServer.readCommandDependencies()
	dependencies.server = s
	dependencies.lifecycle = s
	dependencies.aggregates = s
	return dependencies
}

func TestMarkerlessReadRechecksForDelayedOldClientFrame(t *testing.T) {
	aggregate, err := newHandlerTestAggregate(
		"from STATS select count($time),$time group by $time interval 3600", "default")
	if err != nil {
		t.Fatalf("create aggregate: %v", err)
	}
	t.Cleanup(aggregate.Abort)
	server := &markerlessInputTestServer{
		globCapTestServer: newGlobCapTestServer(1),
		aggregate:         aggregate,
	}
	coordinator := newShutdownCoordinator(server, server, readTimings{}, true, aggregate)
	waiting := make(chan struct{})
	release := make(chan struct{})
	coordinator.legacyInputWait = func(ctx context.Context, _ time.Duration) bool {
		close(waiting)
		select {
		case <-release:
			return true
		case <-ctx.Done():
			return false
		}
	}

	done := make(chan struct{})
	go func() {
		coordinator.maybeFinishAggregateInput(context.Background())
		close(done)
	}()
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("markerless completion did not enter compatibility recheck")
	}

	// Model a previous-version client's next frame reaching dispatch after the
	// first read drained. Its synchronous reservation must cancel completion.
	server.AddPendingFiles(1)
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("markerless compatibility recheck did not return")
	}
	if pending, _ := server.PendingAndActive(); pending != 1 {
		t.Fatalf("pending after compatibility recheck = %d, want delayed reservation", pending)
	}
}

func TestMarkerlessReadRecheckStopsOnContextCancellation(t *testing.T) {
	aggregate, err := newHandlerTestAggregate(
		"from STATS select count($time),$time group by $time interval 3600", "default")
	if err != nil {
		t.Fatalf("create aggregate: %v", err)
	}
	t.Cleanup(aggregate.Abort)
	server := &markerlessInputTestServer{
		globCapTestServer: newGlobCapTestServer(1),
		aggregate:         aggregate,
	}
	coordinator := newShutdownCoordinator(server, server, readTimings{}, true, aggregate)
	entered := make(chan struct{})
	coordinator.legacyInputWait = func(ctx context.Context, _ time.Duration) bool {
		close(entered)
		<-ctx.Done()
		return false
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		coordinator.maybeFinishAggregateInput(ctx)
		close(done)
	}()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("aggregate completion recheck ignored context cancellation")
	}
}

func TestUnbatchedReadFinishesImmediatelyAfterPendingInputDrains(t *testing.T) {
	server := newPendingRegistrationTestServer()
	coordinator := newShutdownCoordinator(server, server, readTimings{}, true, server.aggregate)

	coordinator.maybeFinishAggregateInput(context.Background())

	if calls := server.inputFinishedCalls.Load(); calls != 1 {
		t.Fatalf("aggregate input completion calls = %d, want exactly 1", calls)
	}
}

func TestModernBatchOwnedReadDefersAggregateCompletionToBatch(t *testing.T) {
	server := newPendingRegistrationTestServer()
	coordinator := newShutdownCoordinator(server, server, readTimings{}, true, server.aggregate)
	coordinator.inputBatchOwned = true

	coordinator.maybeFinishAggregateInput(context.Background())

	if calls := server.inputFinishedCalls.Load(); calls != 0 {
		t.Fatalf("batch-owned read finished aggregate input %d times, want 0", calls)
	}
}

package handlers

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/lcontext"
	mapaggregate "github.com/mimecast/dtail/internal/mapr/aggregate"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
)

func TestServerlessCommandBatchDefersIdleShutdownUntilAllCommandsAreAdmitted(t *testing.T) {
	handler := newMapTestHandler(t)
	readServerMessage(t, handler.serverMessages) // Initial capability advertisement.

	var processed []string
	handler.commands["probe"] = func(_ context.Context, _ lcontext.LContext, _ int, args []string, commandFinished func()) {
		processed = append(processed, args[1])
		commandFinished()
	}

	if _, err := handler.Write([]byte(encodeTestCommand(protocol.InputBatchBeginCommand))); err != nil {
		t.Fatalf("write input-batch begin marker: %v", err)
	}
	if _, err := handler.Write([]byte(encodeTestCommand("probe first"))); err != nil {
		t.Fatalf("write first command: %v", err)
	}
	select {
	case <-handler.Done():
		t.Fatal("first completed command shut down the handler before the batch was admitted")
	default:
	}

	if _, err := handler.Write([]byte(encodeTestCommand("probe second"))); err != nil {
		t.Fatalf("write second command: %v", err)
	}
	if len(processed) != 2 || processed[0] != "first" || processed[1] != "second" {
		t.Fatalf("processed commands = %v, want [first second]", processed)
	}

	if _, err := handler.Write([]byte(encodeTestCommand(protocol.InputBatchCompleteCommand))); err != nil {
		t.Fatalf("write input-complete marker: %v", err)
	}
	for {
		if message := readServerMessage(t, handler.serverMessages); message == ".syn close connection" {
			break
		}
	}
	handler.handleAckCommand(3, []string{".ack", "close", "connection"})

	select {
	case <-handler.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("handler did not shut down after the complete batch became idle")
	}
}

func TestCommandBatchFinishesAggregateAfterMarkerWhenReadsAlreadyComplete(t *testing.T) {
	var batch commandBatch
	aggregate := &mapaggregate.Aggregate{}

	batch.begin()
	read := batch.beginRead(omode.CatClient, aggregate)
	if !read.tracked {
		t.Fatal("cat read was not accounted as one-shot batch input")
	}
	if ready := batch.completeRead(read); ready != nil {
		t.Fatal("aggregate input completed before FIFO marker")
	}

	ready := batch.complete()
	if len(ready) != 1 || ready[0] != aggregate {
		t.Fatalf("aggregates ready after marker = %v, want [%p]", ready, aggregate)
	}
}

func TestCommandBatchFinishesAggregateWhenFinalReadCompletesAfterMarker(t *testing.T) {
	var batch commandBatch
	aggregate := &mapaggregate.Aggregate{}

	batch.begin()
	read := batch.beginRead(omode.GrepClient, aggregate)
	if ready := batch.complete(); len(ready) != 0 {
		t.Fatalf("aggregates ready with an active read = %v, want none", ready)
	}

	if ready := batch.completeRead(read); ready != aggregate {
		t.Fatalf("aggregate ready after final read = %p, want %p", ready, aggregate)
	}
}

func TestCommandBatchBeginIsIdempotentWhileReadsAreActive(t *testing.T) {
	var batch commandBatch
	aggregate := &mapaggregate.Aggregate{}

	batch.begin()
	read := batch.beginRead(omode.CatClient, aggregate)
	batch.begin()
	if ready := batch.complete(); len(ready) != 0 {
		t.Fatalf("duplicate begin discarded active read accounting: %v", ready)
	}
	if ready := batch.completeRead(read); ready != aggregate {
		t.Fatalf("aggregate after idempotent begin = %p, want %p", ready, aggregate)
	}
}

func TestCommandBatchMarkerAndFinalReadRaceCompletesAggregateOnce(t *testing.T) {
	for range 1000 {
		var batch commandBatch
		aggregate := &mapaggregate.Aggregate{}
		batch.begin()
		read := batch.beginRead(omode.CatClient, aggregate)

		ready := make(chan *mapaggregate.Aggregate, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for _, completed := range batch.complete() {
				ready <- completed
			}
		}()
		go func() {
			defer wg.Done()
			if completed := batch.completeRead(read); completed != nil {
				ready <- completed
			}
		}()
		wg.Wait()
		close(ready)

		var completed []*mapaggregate.Aggregate
		for item := range ready {
			completed = append(completed, item)
		}
		if len(completed) != 1 || completed[0] != aggregate {
			t.Fatalf("aggregate completions = %v, want exactly [%p]", completed, aggregate)
		}
	}
}

func TestCommandBatchDoesNotFinishFollowInput(t *testing.T) {
	var batch commandBatch
	aggregate := &mapaggregate.Aggregate{}

	batch.begin()
	if read := batch.beginRead(omode.TailClient, aggregate); read.tracked {
		t.Fatal("tail read was accounted as one-shot batch input")
	}
	if ready := batch.complete(); len(ready) != 0 {
		t.Fatalf("follow aggregates ready at batch marker = %v, want none", ready)
	}
}

func TestCommandBatchDoesNotOwnPostMarkerOrReplacementAggregate(t *testing.T) {
	var batch commandBatch
	oldAggregate := &mapaggregate.Aggregate{}
	replacementAggregate := &mapaggregate.Aggregate{}

	batch.begin()
	oldRead := batch.beginRead(omode.CatClient, oldAggregate)
	if ready := batch.complete(); len(ready) != 0 {
		t.Fatalf("aggregates ready while old read remains active = %v, want none", ready)
	}
	if read := batch.beginRead(omode.CatClient, replacementAggregate); read.tracked {
		t.Fatal("post-marker interactive read was captured by the initial batch")
	}
	if batch.ownsAggregate(replacementAggregate) {
		t.Fatal("initial batch owned a replacement aggregate")
	}
	if ready := batch.completeRead(oldRead); ready != oldAggregate {
		t.Fatalf("stale read completed aggregate %p, want old aggregate %p", ready, oldAggregate)
	}
	if batch.ownsAggregate(oldAggregate) {
		t.Fatal("initial batch did not retire after its final tracked read")
	}
}

type recordingBatchCoordinatorServer struct {
	*globCapTestServer
	coordinationCalls atomic.Int32
	coordinated       atomic.Pointer[mapaggregate.Aggregate]
	current           *mapaggregate.Aggregate
}

func (s *recordingBatchCoordinatorServer) Aggregate() *mapaggregate.Aggregate {
	return s.current
}

func (s *recordingBatchCoordinatorServer) readCommandDependencies() readCommandDependencies {
	dependencies := s.globCapTestServer.readCommandDependencies()
	dependencies.server = s
	dependencies.lifecycle = s
	dependencies.aggregates = s
	return dependencies
}

func (s *recordingBatchCoordinatorServer) coordinateAggregateInputCompletion(aggregate *mapaggregate.Aggregate) bool {
	s.coordinationCalls.Add(1)
	s.coordinated.Store(aggregate)
	return true
}

func TestShutdownCoordinatorDelegatesAggregateCompletionToCommandBatch(t *testing.T) {
	admittedAggregate := &mapaggregate.Aggregate{}
	replacementAggregate := &mapaggregate.Aggregate{}
	server := &recordingBatchCoordinatorServer{
		globCapTestServer: newGlobCapTestServer(1),
		current:           replacementAggregate,
	}
	coordinator := newShutdownCoordinator(server, server, readTimings{}, true, admittedAggregate)

	coordinator.maybeFinishAggregateInput(context.Background())

	if got := server.coordinationCalls.Load(); got != 1 {
		t.Fatalf("batch coordination calls = %d, want 1", got)
	}
	if got := server.coordinated.Load(); got != admittedAggregate {
		t.Fatalf("coordinator delegated aggregate %p, want admitted aggregate %p", got, admittedAggregate)
	}
}

type aggregateSwapReadServer struct {
	*globCapTestServer
	current atomic.Pointer[mapaggregate.Aggregate]
}

func (s *aggregateSwapReadServer) Aggregate() *mapaggregate.Aggregate {
	return s.current.Load()
}

func (s *aggregateSwapReadServer) readCommandDependencies() readCommandDependencies {
	dependencies := s.globCapTestServer.readCommandDependencies()
	dependencies.server = s
	dependencies.lifecycle = s
	dependencies.aggregates = s
	return dependencies
}

func TestReadCommandProcessorUsesAggregateCapturedAtAdmission(t *testing.T) {

	oldAggregate, err := newHandlerTestAggregate(
		"from STATS select count($time),$time group by $time interval 3600", "default")
	if err != nil {
		t.Fatalf("create old aggregate: %v", err)
	}
	replacementAggregate, err := newHandlerTestAggregate(
		"from STATS select count($time),$time group by $time interval 3600", "default")
	if err != nil {
		t.Fatalf("create replacement aggregate: %v", err)
	}

	readServer := &aggregateSwapReadServer{globCapTestServer: newGlobCapTestServer(1)}
	readServer.current.Store(oldAggregate)
	command := newReadCommand(readServer, omode.CatClient)
	// Model SESSION UPDATE while the admitted read is paused before processor
	// construction. The read must remain bound to the old generation.
	readServer.current.Store(replacementAggregate)

	processor := command.makeProcessor("old.log", "old.log", nil)
	if err := processor.ProcessLine(bytes.NewBufferString(testStatsLine), 1, "old.log"); err != nil {
		t.Fatalf("process old-generation line: %v", err)
	}
	if err := processor.Close(); err != nil {
		t.Fatalf("close old-generation processor: %v", err)
	}

	oldOutput := make(chan string, 4)
	oldAggregate.PrepareOutput(context.Background(), oldOutput)
	oldAggregate.Shutdown(context.Background())
	select {
	case result := <-oldOutput:
		if !strings.Contains(result, "count($time)≔1") {
			t.Fatalf("old aggregate result = %q, want count 1", result)
		}
	default:
		t.Fatal("admitted aggregate received no result")
	}

	replacementOutput := make(chan string, 4)
	replacementAggregate.PrepareOutput(context.Background(), replacementOutput)
	replacementAggregate.Shutdown(context.Background())
	select {
	case result := <-replacementOutput:
		t.Fatalf("replacement aggregate received stale read result %q", result)
	default:
	}
}

func TestServerlessMapBatchAcceptsReadAfterEarlierReadFullyCompletes(t *testing.T) {
	handler := newMapTestHandler(t)
	firstPath := writeTestStatsFile(t, 1)
	secondPath := writeTestStatsFile(t, 2)
	// writeTestStatsFile uses a fresh TempDir on each call, so the two paths
	// are distinct even though they share the same base name.

	commandWg := wrapHandlerCommandsForJoin(handler)
	output := &testOutput{}
	var writeMu sync.Mutex
	readerDone := startTestReader(handler, output, &writeMu)

	writeMu.Lock()
	_, beginErr := handler.Write([]byte(encodeTestCommand(protocol.InputBatchBeginCommand)))
	writeMu.Unlock()
	if beginErr != nil {
		t.Fatalf("write input-batch begin marker: %v", beginErr)
	}
	query := "from STATS select count($time),$time group by $time interval 3600"
	writeCommand := func(command string) {
		t.Helper()
		writeMu.Lock()
		_, err := handler.Write([]byte(encodeTestCommand(command)))
		writeMu.Unlock()
		if err != nil {
			t.Fatalf("write %q: %v", command, err)
		}
	}

	writeCommand("map:plain=true:serverless=true " + query)
	writeCommand(fmt.Sprintf("cat:plain=true:serverless=true %s .", firstPath))

	// Hold the second command outside the server until the first read command
	// has returned completely. At this point the map command is the sole active
	// command. The old pending-file grace path called FinishInput here, before
	// the FIFO marker proved that the initial command stream was complete.
	waitForHandlerCondition(t, 5*time.Second, "first read did not reach map-only state", func() bool {
		pending, active := handler.PendingAndActive()
		aggregate := handler.getAggregate()
		return aggregate != nil && handler.commandBatch.ownsAggregate(aggregate) && pending == 0 && active == 1
	}, func() string {
		pending, active := handler.PendingAndActive()
		aggregate := handler.getAggregate()
		return fmt.Sprintf("pending=%d active=%d aggregatePresent=%t batchOwnsAggregate=%t",
			pending, active, aggregate != nil, handler.commandBatch.ownsAggregate(aggregate))
	})

	writeCommand("cat:invalid-option ignored .")
	if pending, active := handler.PendingAndActive(); pending != 0 || active != 1 {
		t.Fatalf("invalid option changed batch state: pending %d, active %d", pending, active)
	}
	if aggregate := handler.getAggregate(); aggregate == nil || !handler.commandBatch.ownsAggregate(aggregate) {
		t.Fatal("invalid option released the earlier read's batch ownership")
	}

	writeCommand(fmt.Sprintf("cat:plain=true:serverless=true %s .", secondPath))
	writeCommand(protocol.InputBatchCompleteCommand)

	select {
	case <-handler.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("serverless map batch did not shut down after the final read")
	}
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not observe EOF after serverless map shutdown")
	}
	waitForCommandJoin(t, commandWg, 5*time.Second)

	if got := output.String(); !strings.Contains(got, "count($time)≔3") {
		t.Fatalf("final aggregate omitted a read admitted after an earlier read completed: %q", got)
	}
}

func TestInputBatchKeepsUnclaimedReadFromFinishingBeforeLaterValidRead(t *testing.T) {
	handler := newMapTestHandler(t)
	path := writeTestStatsFile(t, 3)
	readServerMessage(t, handler.serverMessages) // Initial capability advertisement.

	commandWg := wrapHandlerCommandsForJoin(handler)
	validCat := handler.commands["cat"]
	handler.commands["cat"] = func(ctx context.Context, ltx lcontext.LContext,
		argc int, args []string, commandFinished func()) {

		if args[1] == "unclaimed" {
			commandFinished()
			return
		}
		validCat(ctx, ltx, argc, args, commandFinished)
	}

	writeCommand := func(command string) {
		t.Helper()
		if _, err := handler.Write([]byte(encodeTestCommand(command))); err != nil {
			t.Fatalf("write %q: %v", command, err)
		}
	}
	writeCommand(protocol.InputBatchBeginCommand)
	query := "from STATS select count($time),$time group by $time interval 3600"
	writeCommand("map:plain=true:serverless=true " + query)
	writeCommand("cat:plain=true:serverless=true unclaimed .")

	aggregate := handler.getAggregate()
	if aggregate == nil {
		t.Fatal("map command did not publish its aggregate")
	}
	if pending, active := handler.PendingAndActive(); pending != 0 || active != 1 {
		t.Fatalf("state after unclaimed read = pending %d, active %d; want map only", pending, active)
	}
	if !handler.commandBatch.ownsAggregate(aggregate) {
		t.Fatal("unclaimed read released aggregate ownership before the batch marker")
	}

	writeCommand(fmt.Sprintf("cat:plain=true:serverless=true %s .", path))
	writeCommand(protocol.InputBatchCompleteCommand)

	result := readServerMessage(t, handler.maprMessages)
	if !strings.Contains(result, "count($time)≔3") {
		t.Fatalf("final aggregate omitted the valid read after an unclaimed read: %q", result)
	}
	for {
		if message := readServerMessage(t, handler.serverMessages); message == ".syn close connection" {
			break
		}
	}
	handler.handleAckCommand(3, []string{".ack", "close", "connection"})
	select {
	case <-handler.Done():
	case <-time.After(time.Second):
		t.Fatal("handler did not shut down after input batch completion")
	}
	waitForCommandJoin(t, commandWg, 5*time.Second)
}

func TestSessionCommandBatchContextsKeepOverlappingGenerationsSeparate(t *testing.T) {
	handler := newMapTestHandler(t)
	readServerMessage(t, handler.serverMessages) // Initial capability advertisement.
	handler.sessionState.mu.Lock()
	handler.sessionState.active = true
	handler.sessionState.mu.Unlock()

	query := "from STATS select count($time),$time group by $time interval 3600"
	firstAggregate, err := newHandlerTestAggregate(query, "default")
	if err != nil {
		t.Fatalf("create first aggregate: %v", err)
	}
	secondAggregate, err := newHandlerTestAggregate(query, "default")
	if err != nil {
		t.Fatalf("create second aggregate: %v", err)
	}
	t.Cleanup(firstAggregate.Abort)
	t.Cleanup(secondAggregate.Abort)

	var batches []*commandBatch
	var aggregates []*mapaggregate.Aggregate
	var reads []*readCommand
	var finishes []func()
	handler.commands["cat"] = func(ctx context.Context, _ lcontext.LContext,
		_ int, _ []string, commandFinished func()) {

		reservation := pendingInputReservationFromContext(ctx)
		batches = append(batches, reservation.inputBatch)
		aggregates = append(aggregates, reservation.aggregate)
		command := newReadCommandWithAggregate(handler, omode.CatClient, reservation.aggregate)
		command.adoptPendingInputReservation(reservation)
		reads = append(reads, command)
		finishes = append(finishes, commandFinished)
	}

	handler.setAggregate(firstAggregate)
	if err := handler.dispatchSessionCommands(context.Background(), []string{"cat first.log ."}); err != nil {
		t.Fatalf("dispatch first session generation: %v", err)
	}
	handler.setAggregate(secondAggregate)
	if err := handler.dispatchSessionCommands(context.Background(), []string{"cat second.log ."}); err != nil {
		t.Fatalf("dispatch second session generation: %v", err)
	}

	if len(batches) != 2 {
		t.Fatalf("captured session batch count = %d, want 2", len(batches))
	}
	if batches[0] == batches[1] {
		t.Fatalf("session batch identities = %p, %p; want distinct generation-local batches", batches[0], batches[1])
	}
	if len(aggregates) != 2 || aggregates[0] != firstAggregate || aggregates[1] != secondAggregate {
		t.Fatalf("captured aggregates = %v, want [%p %p]", aggregates, firstAggregate, secondAggregate)
	}
	if pending, active := handler.PendingAndActive(); pending != 2 || active != 2 {
		t.Fatalf("overlapping session state = pending %d, active %d; want 2, 2", pending, active)
	}

	reads[0].releasePendingInputReservation(context.Background())
	reads[0].completeInputBatch()
	finishes[0]()
	reads[1].releasePendingInputReservation(context.Background())
	reads[1].completeInputBatch()
	finishes[1]()
	if pending, active := handler.PendingAndActive(); pending != 0 || active != 0 {
		t.Fatalf("session batch counters = pending %d, active %d; want zero", pending, active)
	}
}

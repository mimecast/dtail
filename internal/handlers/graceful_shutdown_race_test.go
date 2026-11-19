package handlers

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/lcontext"
	mapaggregate "github.com/mimecast/dtail/internal/mapr/aggregate"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/session"
)

func TestGracefulShutdownWaitsForAdmittedAggregatePublication(t *testing.T) {
	handler := newMapTestHandler(t)
	aggregate := newPopulatedTestAggregate(t, 1)

	commandEntered := make(chan struct{})
	publish := make(chan struct{})
	handler.commands["map"] = func(ctx context.Context, _ lcontext.LContext, _ int, _ []string, commandFinished func()) {
		close(commandEntered)
		<-publish
		messages, closeMessages := handler.newGeneratedMaprMessagesChannel(0)
		aggregate.PrepareOutput(context.Background(), messages)
		handler.setAggregate(aggregate)
		go func() {
			aggregate.Start(ctx, messages)
			closeMessages()
			commandFinished()
		}()
	}

	commandDone := make(chan struct{})
	go func() {
		defer close(commandDone)
		handler.handleUserCommand(context.Background(), lcontext.LContext{}, 1, []string{"map"}, "map")
	}()
	waitForGracefulSignal(t, commandEntered, "map command initialization")

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		handler.GracefulShutdownContext(context.Background())
	}()
	select {
	case <-shutdownDone:
		t.Fatal("graceful shutdown returned before admitted map command published its aggregate")
	case <-time.After(50 * time.Millisecond):
	}

	close(publish)
	waitForGracefulSignal(t, commandDone, "map command publication")
	waitForGracefulSignal(t, shutdownDone, "graceful shutdown")

	select {
	case message := <-handler.maprMessages:
		_, payload := decodeGeneratedMessage(message)
		if !strings.Contains(payload, "≔1") {
			t.Fatalf("final aggregate payload = %q, want one sample", payload)
		}
	default:
		t.Fatal("graceful shutdown lost the admitted aggregate's final output")
	}
}

func TestGracefulShutdownContextAbortsWhenFinalOutputCannotDrain(t *testing.T) {
	handler := newMapTestHandler(t)
	groupCount := cap(handler.maprMessages) + 32
	aggregate := newPopulatedTestAggregate(t, groupCount)
	messages, closeMessages := handler.newGeneratedMaprMessagesChannel(0)
	aggregate.PrepareOutput(context.Background(), messages)
	handler.setAggregate(aggregate)

	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		aggregate.Start(context.Background(), messages)
		closeMessages()
	}()

	drainCtx, cancelDrain := context.WithCancel(context.Background())
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		handler.GracefulShutdownContext(drainCtx)
	}()

	waitForHandlerCondition(t, 2*time.Second, "final output did not fill protocol queue", func() bool {
		return len(handler.maprMessages) == cap(handler.maprMessages)
	}, func() string {
		return fmt.Sprintf("queue length=%d capacity=%d", len(handler.maprMessages), cap(handler.maprMessages))
	})
	cancelDrain()
	waitForGracefulSignal(t, shutdownDone, "canceled graceful shutdown")
	waitForGracefulSignal(t, startDone, "aggregate producer shutdown")

	if len(handler.maprMessages) != cap(handler.maprMessages) {
		t.Fatalf("protocol queue length = %d, want full capacity %d", len(handler.maprMessages), cap(handler.maprMessages))
	}
}

func TestGracefulShutdownLetsAdmittedSessionUpdateDispatchReplacement(t *testing.T) {
	handler := newSessionTestHandler("session-update-shutdown-user")
	handler.handleCommandCb = handler.handleUserCommand
	readServerMessage(t, handler.serverMessages)

	dispatched := make(chan recordedCommand, 2)
	handler.commands["tail"] = func(ctx context.Context, _ lcontext.LContext, _ int, args []string, commandFinished func()) {
		dispatched <- recordedCommand{command: strings.Join(args, " "), ctx: ctx}
		commandFinished()
	}
	handler.commands["SESSION"] = handler.handleSessionCommand

	startPayload := mustSessionPayload(t, session.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/old.log"},
		Regex: ".",
	})
	handler.handleUserCommand(context.Background(), lcontext.LContext{}, 3,
		[]string{"SESSION", "START", startPayload}, "SESSION")
	if message := readServerMessage(t, handler.serverMessages); message != sessionAckStartOKPrefix+" 1" {
		t.Fatalf("unexpected session start ack: %q", message)
	}
	old := waitForRecordedCommand(t, dispatched)

	realSessionHandler := handler.handleSessionCommand
	updatePaused := make(chan struct{})
	resumeUpdate := make(chan struct{})
	handler.commands["SESSION"] = func(ctx context.Context, ltx lcontext.LContext, argc int, args []string, commandFinished func()) {
		close(updatePaused)
		<-resumeUpdate
		realSessionHandler(ctx, ltx, argc, args, commandFinished)
	}
	updatePayload := mustSessionPayload(t, session.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/replacement.log"},
		Regex: ".",
	})
	updateDone := make(chan struct{})
	go func() {
		defer close(updateDone)
		handler.handleUserCommand(context.Background(), lcontext.LContext{}, 3,
			[]string{"SESSION", "UPDATE", updatePayload}, "SESSION")
	}()
	waitForGracefulSignal(t, updatePaused, "admitted session update")

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		handler.GracefulShutdownContext(context.Background())
	}()
	waitForHandlerStopping(t, handler)
	close(resumeUpdate)

	waitForGracefulSignal(t, updateDone, "session update completion")
	replacement := waitForRecordedCommand(t, dispatched)
	if !strings.Contains(replacement.command, "/var/log/replacement.log") {
		t.Fatalf("replacement command = %q, want replacement workload", replacement.command)
	}
	if message := readServerMessage(t, handler.serverMessages); message != sessionAckUpdateOKPrefix+" 2" {
		t.Fatalf("unexpected session update ack: %q", message)
	}
	waitForContextDone(old.ctx, t)
	waitForGracefulSignal(t, shutdownDone, "graceful shutdown after session update")
}

func TestGracefulShutdownCanceledWhileInitializerBlockedOnFullQueue(t *testing.T) {
	handler := newSessionTestHandler("blocked-init-shutdown-user")
	readServerMessage(t, handler.serverMessages)

	for len(handler.serverMessages) < cap(handler.serverMessages) {
		handler.serverMessages <- "queued"
	}
	initializerEntered := make(chan struct{})
	handler.commands["block"] = func(_ context.Context, _ lcontext.LContext, _ int, _ []string, commandFinished func()) {
		close(initializerEntered)
		handler.send(handler.serverMessages, "blocked")
		commandFinished()
	}
	commandDone := make(chan struct{})
	go func() {
		defer close(commandDone)
		handler.handleUserCommand(context.Background(), lcontext.LContext{}, 1, []string{"block"}, "block")
	}()
	waitForGracefulSignal(t, initializerEntered, "blocked command initializer")

	drainCtx, cancelDrain := context.WithCancel(context.Background())
	cancelDrain()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		handler.GracefulShutdownContext(drainCtx)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		// Release the initializer so a failure does not leak the test goroutine.
		handler.Shutdown()
		t.Fatal("canceled graceful shutdown waited on a blocked initializer")
	}
	waitForGracefulSignal(t, commandDone, "blocked initializer cancellation")
}

func TestGracefulShutdownAcceptsCloseAckAfterAdmissionSealed(t *testing.T) {
	handler := newSessionTestHandler("shutdown-ack-user")
	readServerMessage(t, handler.serverMessages)
	handler.commands["cat"] = func(_ context.Context, _ lcontext.LContext, _ int, _ []string, commandFinished func()) {
		commandFinished()
	}

	commandDone := make(chan struct{})
	go func() {
		defer close(commandDone)
		handler.handleUserCommand(context.Background(), lcontext.LContext{}, 1, []string{"cat"}, "cat")
	}()
	if message := readServerMessage(t, handler.serverMessages); message != ".syn close connection" {
		t.Fatalf("unexpected shutdown message: %q", message)
	}

	gracefulDone := make(chan struct{})
	go func() {
		defer close(gracefulDone)
		handler.GracefulShutdownContext(context.Background())
	}()
	waitForHandlerStopping(t, handler)
	handler.handleUserCommand(context.Background(), lcontext.LContext{}, 3,
		[]string{".ack", "close", "connection"}, ".ack")

	waitForGracefulSignal(t, commandDone, "shutdown command close acknowledgement")
	waitForGracefulSignal(t, gracefulDone, "graceful shutdown after close acknowledgement")
}

func newPopulatedTestAggregate(t *testing.T, groups int) *mapaggregate.Aggregate {
	t.Helper()

	aggregate, err := newHandlerTestAggregate(
		"from STATS select count($time),$time from - group by $time interval 3600",
		"default",
	)
	if err != nil {
		t.Fatalf("create aggregate: %v", err)
	}
	processor := mapaggregate.NewProcessor(aggregate, "-")
	for i := 0; i < groups; i++ {
		line := strings.Replace(testStatsLine, "1002-071143", fmt.Sprintf("1002-%06d", i), 1)
		if err := processor.ProcessLine(bytes.NewBufferString(line), uint64(i+1), "-"); err != nil {
			t.Fatalf("process aggregate line %d: %v", i, err)
		}
	}
	if err := processor.Close(); err != nil {
		t.Fatalf("close aggregate processor: %v", err)
	}
	return aggregate
}

func waitForGracefulSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitForRecordedCommand(t *testing.T, commands <-chan recordedCommand) recordedCommand {
	t.Helper()
	select {
	case command := <-commands:
		return command
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatched session command")
		return recordedCommand{}
	}
}

func waitForHandlerStopping(t *testing.T, handler *ServerHandler) {
	t.Helper()
	waitForHandlerCondition(t, 2*time.Second, "handler did not seal command admission", handler.isStopping)
}

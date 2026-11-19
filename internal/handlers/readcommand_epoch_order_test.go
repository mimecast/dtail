package handlers

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

type batchCompletionTestServer struct {
	*globCapTestServer

	mu          sync.Mutex
	finishCalls int
	finishMode  omode.Mode
	generation  uint64
}

func (s *batchCompletionTestServer) readCommandDependencies() readCommandDependencies {
	dependencies := s.globCapTestServer.readCommandDependencies()
	dependencies.server = s
	dependencies.lifecycle = s
	dependencies.aggregates = s
	return dependencies
}

func (s *batchCompletionTestServer) FinishReadBatch(_ context.Context, mode omode.Mode, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishCalls++
	s.finishMode = mode
	s.generation = generation
}

func TestReadFilesDelegatesBatchCompletionToHandler(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create temp file: %v", err)
	}

	server := &batchCompletionTestServer{globCapTestServer: newGlobCapTestServer(100)}
	command := newReadCommand(server, omode.CatClient)
	command.generation = 42
	command.readFiles(context.Background(), lcontext.LContext{}, []string{path}, path, regex.NewNoop())

	server.mu.Lock()
	defer server.mu.Unlock()
	if server.finishCalls != 1 {
		t.Fatalf("FinishReadBatch calls = %d, want 1", server.finishCalls)
	}
	if server.finishMode != omode.CatClient || server.generation != 42 {
		t.Fatalf("FinishReadBatch args = (%v, %d), want (%v, 42)",
			server.finishMode, server.generation, omode.CatClient)
	}
}

type orderedBatchCompletionState struct {
	calls             []string
	signaledEpoch     uint64
	flushedGeneration uint64
}

func (s *orderedBatchCompletionState) record(call string) {
	s.calls = append(s.calls, call)
}

func (s *orderedBatchCompletionState) DirectOutputActive() bool {
	s.record("DirectOutputActive")
	return true
}

func (s *orderedBatchCompletionState) HasOutputEOF() bool {
	s.record("HasOutputEOF")
	return true
}

func (s *orderedBatchCompletionState) OutputEpoch() uint64 {
	s.record("OutputEpoch")
	return 42
}

func (s *orderedBatchCompletionState) PendingAndActive() (int32, int32) {
	s.record("PendingAndActive")
	return 0, 1
}

func (s *orderedBatchCompletionState) traceSkippedReadBatchEOF(int32, int32) {
	s.record("traceSkippedReadBatchEOF")
}

func (s *orderedBatchCompletionState) flushReadBatch(_ context.Context, generation uint64) bool {
	s.record("flushReadBatch")
	s.flushedGeneration = generation
	return true
}

func (s *orderedBatchCompletionState) SignalOutputEOF(epoch uint64) {
	s.record("SignalOutputEOF")
	s.signaledEpoch = epoch
}

func (s *orderedBatchCompletionState) waitForReadBatchAck(context.Context) {
	s.record("waitForReadBatchAck")
}

// The epoch must be captured before the pending-work snapshot. A read joining
// after the epoch capture advances that epoch, so SignalOutputEOF safely drops
// this batch's stale signal even if the join was invisible to the snapshot.
func TestFinishReadBatchCapturesEpochBeforePendingCheck(t *testing.T) {
	state := &orderedBatchCompletionState{}

	finishReadBatch(context.Background(), omode.CatClient, 7, state)

	want := []string{
		"DirectOutputActive",
		"HasOutputEOF",
		"OutputEpoch",
		"PendingAndActive",
		"flushReadBatch",
		"SignalOutputEOF",
		"waitForReadBatchAck",
	}
	if strings.Join(state.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("batch completion calls = %v, want %v", state.calls, want)
	}
	if state.flushedGeneration != 7 {
		t.Fatalf("flushed generation = %d, want 7", state.flushedGeneration)
	}
	if state.signaledEpoch != 42 {
		t.Fatalf("signaled epoch = %d, want captured epoch 42", state.signaledEpoch)
	}
}

func TestFinishReadBatchSignalsEOFWhenPendingWorkIsDrained(t *testing.T) {
	handler := &ServerHandler{
		baseHandler: newBaseHandler(context.Background(), baseHandlerConfig{serverlessOutput: io.Discard}),
		readTimings: newReadTimings(nil),
	}
	handler.output.configure(outputManagerConfig{}, nil)
	handler.EnableDirectOutput()

	handler.FinishReadBatch(context.Background(), omode.CatClient, 1)

	handler.output.mu.Lock()
	eof := handler.output.eof
	handler.output.mu.Unlock()
	select {
	case <-eof:
	default:
		t.Fatal("FinishReadBatch did not signal EOF for the final read")
	}
}

func TestFinishReadBatchDoesNotSignalEOFWithPendingWork(t *testing.T) {
	handler := &ServerHandler{
		baseHandler:  newBaseHandler(context.Background(), baseHandlerConfig{serverlessOutput: io.Discard}),
		readTimings:  newReadTimings(nil),
		pendingFiles: 1,
	}
	handler.output.configure(outputManagerConfig{}, nil)
	handler.EnableDirectOutput()

	handler.FinishReadBatch(context.Background(), omode.CatClient, 1)

	handler.output.mu.Lock()
	eof := handler.output.eof
	handler.output.mu.Unlock()
	select {
	case <-eof:
		t.Fatal("FinishReadBatch signaled EOF while another read was pending")
	default:
	}
}

func TestFinishReadBatchReportsFlushFailureAndStillSignalsEOF(t *testing.T) {
	handler := &ServerHandler{
		baseHandler: newBaseHandler(context.Background(), baseHandlerConfig{serverlessOutput: io.Discard}),
		readTimings: newReadTimings(nil),
	}
	handler.output.configure(outputManagerConfig{flushTimeout: time.Millisecond}, nil)
	handler.EnableDirectOutput()
	if err := handler.output.enqueue(context.Background(), 7, []byte("blocked"), nil); err != nil {
		t.Fatalf("queue output: %v", err)
	}

	handler.FinishReadBatch(context.Background(), omode.CatClient, 7)

	select {
	case raw := <-handler.flushErrors:
		generation, message := decodeGeneratedMessage(raw)
		if generation != 7 {
			t.Fatalf("flush error generation = %d, want 7", generation)
		}
		if !strings.Contains(message, errOutputFlushTimeout.Error()) {
			t.Fatalf("flush error message = %q, want %q", message, errOutputFlushTimeout)
		}
	default:
		t.Fatal("FinishReadBatch did not report the output flush failure")
	}

	handler.output.mu.Lock()
	eof := handler.output.eof
	handler.output.mu.Unlock()
	select {
	case <-eof:
	default:
		t.Fatal("FinishReadBatch did not signal EOF after reporting the flush failure")
	}
}

var _ readCommandServer = (*batchCompletionTestServer)(nil)
var _ readCommandDependencyProvider = (*batchCompletionTestServer)(nil)

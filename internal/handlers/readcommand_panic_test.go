package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	mapaggregate "github.com/mimecast/dtail/internal/mapr/aggregate"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

type panicReadLogger struct {
	errors chan string
}

func (l *panicReadLogger) Error(args ...any) string {
	message := fmt.Sprint(args...)
	select {
	case l.errors <- message:
	default:
	}
	return message
}
func (*panicReadLogger) Warn(...any) string  { return "" }
func (*panicReadLogger) Info(...any) string  { return "" }
func (*panicReadLogger) Debug(...any) string { return "" }
func (*panicReadLogger) Trace(...any) string { return "" }
func (*panicReadLogger) TraceEnabled() bool  { return false }

type panicReadServer struct {
	*globCapTestServer
	logger  *panicReadLogger
	aborted atomic.Bool
}

type panickingFileReader struct{}

func (panickingFileReader) Start(context.Context, lcontext.LContext,
	line.Processor, regex.Regex) error {
	panic("reader processor path failed")
}

func (panickingFileReader) FilePath() string { return "panic.log" }
func (panickingFileReader) Retry() bool      { return false }

type panicAggregateServer struct {
	*globCapTestServer
	aggregate *mapaggregate.Aggregate
}

func (s *panicAggregateServer) Aggregate() *mapaggregate.Aggregate { return s.aggregate }

func (s *panicReadServer) PrepareReadTarget(string) (fs.ValidatedReadTarget, error) {
	panic("prepare target failed")
}

func (s *panicReadServer) Logger() logging.Logger { return s.logger }
func (s *panicReadServer) abortAfterPanic()       { s.aborted.Store(true) }

func (s *panicReadServer) readCommandDependencies() readCommandDependencies {
	dependencies := s.globCapTestServer.readCommandDependencies()
	dependencies.server = s
	dependencies.lifecycle = s
	dependencies.aggregates = s
	dependencies.logger = s.logger
	dependencies.abortAfterPanic = s.abortAfterPanic
	return dependencies
}

func (s *panicAggregateServer) readCommandDependencies() readCommandDependencies {
	dependencies := s.globCapTestServer.readCommandDependencies()
	dependencies.server = s
	dependencies.lifecycle = s
	dependencies.aggregates = s
	return dependencies
}

func TestReadFileGoroutineRecoversPanicAndReleasesAccounting(t *testing.T) {
	logger := &panicReadLogger{errors: make(chan string, 2)}
	server := &panicReadServer{
		globCapTestServer: newGlobCapTestServer(1),
		logger:            logger,
	}
	command := newReadCommand(server, omode.CatClient)

	command.readFiles(context.Background(), lcontext.LContext{},
		[]string{"panic.log"}, "panic.log", regex.NewNoop())

	if pending, _ := server.PendingAndActive(); pending != 0 {
		t.Fatalf("pending files = %d, want 0 after recovered panic", pending)
	}
	if !server.aborted.Load() {
		t.Fatal("session was not aborted after file goroutine panic")
	}
	select {
	case message := <-logger.errors:
		if !strings.Contains(message, "file read") || !strings.Contains(message, "prepare target failed") {
			t.Fatalf("panic log = %q, want scope and panic value", message)
		}
	default:
		t.Fatal("file goroutine panic was not logged")
	}
}

func TestExecuteReadLoopEscalatesReaderWorkerPanic(t *testing.T) {
	server := newGlobCapTestServer(1)
	command := newReadCommand(server, omode.CatClient)
	reader, err := fs.NewReadFile(fs.ReadOptions{
		Mode:          omode.CatClient,
		GlobID:        "worker-panic",
		MaxLineLength: 1024,
		Logger:        logging.NopLogger{},
	})
	if err != nil {
		t.Fatalf("create file reader: %v", err)
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		command.executeReadLoop(context.Background(), lcontext.LContext{}, "panic.log", "panic.log",
			regex.NewNoop(), reader, func(context.Context, lcontext.LContext, fs.FileReader, regex.Regex) error {
				return fmt.Errorf("truncate failure: %w", fs.ErrReaderWorkerPanic)
			})
	}()
	recoveredErr, ok := recovered.(error)
	if !ok || !errors.Is(recoveredErr, fs.ErrReaderWorkerPanic) {
		t.Fatalf("recovered value = %v, want reader worker panic escalation", recovered)
	}
}

func TestReadViaProcessorClosesAggregateProcessorAfterReaderPanic(t *testing.T) {
	aggregate, err := newHandlerTestAggregate("select count($0) from .", "")
	if err != nil {
		t.Fatalf("create aggregate: %v", err)
	}
	server := &panicAggregateServer{
		globCapTestServer: newGlobCapTestServer(1),
		aggregate:         aggregate,
	}
	command := newReadCommandWithAggregate(server, omode.CatClient, aggregate)
	strategy := command.readViaProcessor("panic.log", "panic.log", nil)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = strategy(context.Background(), lcontext.LContext{}, panickingFileReader{}, regex.NewNoop())
	}()
	if recovered == nil || !strings.Contains(fmt.Sprint(recovered), "reader processor path failed") {
		t.Fatalf("recovered panic = %v, want reader panic", recovered)
	}

	done := make(chan struct{})
	go func() {
		aggregate.AbortAndWait(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Aggregate.AbortAndWait hung after reader panic")
	}
}

var _ readCommandServer = (*panicReadServer)(nil)
var _ fs.FileReader = panickingFileReader{}
var _ readCommandServer = (*panicAggregateServer)(nil)
var _ readCommandDependencyProvider = (*panicReadServer)(nil)
var _ readCommandDependencyProvider = (*panicAggregateServer)(nil)

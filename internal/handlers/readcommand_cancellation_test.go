package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

type retryOnlyFileReader struct{}

func (retryOnlyFileReader) Start(context.Context, lcontext.LContext, line.Processor, regex.Regex) error {
	return nil
}

func (retryOnlyFileReader) FilePath() string {
	return ""
}

func (retryOnlyFileReader) Retry() bool {
	return true
}

var _ fs.FileReader = retryOnlyFileReader{}

func TestExecuteReadLoopStopsPromptlyWhenContextCanceledDuringRetrySleep(t *testing.T) {
	handler := newSessionTestHandler("readcommand-cancel-user")
	handler.readTimings.readRetryInterval = time.Second

	command := newReadCommand(handler, omode.TailClient)
	reader := retryOnlyFileReader{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	strategyCalls := 0

	go func() {
		command.executeReadLoop(ctx, lcontext.LContext{}, "/var/log/app.log", "app.log", regex.NewNoop(), reader,
			func(context.Context, lcontext.LContext, fs.FileReader, regex.Regex) error {
				strategyCalls++
				cancel()
				return nil
			})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("executeReadLoop did not stop promptly after cancellation")
	}

	if strategyCalls != 1 {
		t.Fatalf("expected one read attempt before cancellation, got %d", strategyCalls)
	}
}

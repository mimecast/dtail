//go:build linux

package handlers

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/io/fs"
	journaltest "github.com/mimecast/dtail/internal/io/journal/testhelper"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

// journalWarnLogger formats warnings, so that the test can tell the long-line
// warning from other server messages (logging.NopLogger formats nothing).
type journalWarnLogger struct{ logging.NopLogger }

func (journalWarnLogger) Warn(args ...any) string { return "WARN|" + fmt.Sprint(args...) }

// journalCaptureProcessor records the lines a reader passes on.
type journalCaptureProcessor struct {
	lines []string
}

func (p *journalCaptureProcessor) ProcessLine(line *bytes.Buffer, _ uint64, _ string) error {
	p.lines = append(p.lines, line.String())
	return nil
}

func (*journalCaptureProcessor) Flush() error { return nil }
func (*journalCaptureProcessor) Close() error { return nil }

// TestMakeReaderPassesMaxLineLengthToJournalReader checks that the read
// command's Server.MaxLineLength reaches the journal reader: with a limit far
// below the 1 MiB default, an entry longer than it is cut into pieces of
// exactly that length (and warned about), while a shorter one arrives whole.
// Without the limit the reader would pass both entries on whole.
func TestMakeReaderPassesMaxLineLengthToJournalReader(t *testing.T) {
	journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{Lines: []string{"abcdefghij", "ok"}},
	})
	target, err := fs.NewValidatedJournalTarget("journal:ssh.service")
	if err != nil {
		t.Fatalf("journal target: %v", err)
	}
	messages := make(chan string, 4)
	reader, err := makeReader(readerFactoryOptions{
		target:         &target,
		path:           "journal:ssh.service",
		serverMessages: messages,
		maxLineLength:  4,
		logger:         journalWarnLogger{},
	}, omode.CatClient, false)
	if err != nil {
		t.Fatalf("make reader: %v", err)
	}

	processor := &journalCaptureProcessor{}
	if err := reader.Start(context.Background(), emptyLContext(), processor, regex.NewNoop()); err != nil {
		t.Fatalf("start reader: %v", err)
	}
	want := []string{"abcd", "efgh", "ij\n", "ok\n"}
	if !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("journal lines = %q, want %q", processor.lines, want)
	}
	if len(messages) != 1 {
		t.Fatalf("got %d server messages, want one long-line warning", len(messages))
	}
	if message := <-messages; message != fs.LongLineWarning(journalWarnLogger{}, "journal:ssh.service") ||
		!strings.Contains(message, "Long log line") {
		t.Fatalf("server message = %q, want a long-line warning", message)
	}
}

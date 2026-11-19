//go:build linux

package handlers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/journal"
	journaltest "github.com/mimecast/dtail/internal/io/journal/testhelper"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

type warningTextLogger struct {
	logging.NopLogger
}

func (*warningTextLogger) Warn(args ...any) string {
	return fmt.Sprint(args...)
}

func TestReaderFactoryCreatesJournalSnapshotAndFollowReaders(t *testing.T) {
	journaltest.InstallMock(t, journaltest.Scenario{})
	target, err := fs.NewValidatedJournalTarget("journal:ssh.service")
	if err != nil {
		t.Fatalf("create journal target: %v", err)
	}

	tests := []struct {
		name      string
		mode      omode.Mode
		wantRetry bool
	}{
		{name: "snapshot", mode: omode.CatClient},
		{name: "follow", mode: omode.TailClient, wantRetry: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slots := newRecordingReadSlots()
			reader, limiter, err := readerFactoryFor(tt.mode)(fileReaderFactoryOptions(slots, &target))
			if err != nil {
				t.Fatalf("factory() error = %v", err)
			}
			if _, ok := reader.(*journal.Reader); !ok {
				t.Fatalf("reader type = %T, want *journal.Reader", reader)
			}
			if got := reader.Retry(); got != tt.wantRetry {
				t.Errorf("Retry() = %v, want %v", got, tt.wantRetry)
			}
			if limiter == nil {
				t.Fatal("factory returned nil limiter")
			}
		})
	}
}

func TestReaderFactoryJournalConstructionFailureReturnsNoLimiter(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	target, err := fs.NewValidatedJournalTarget("journal:ssh.service")
	if err != nil {
		t.Fatalf("create journal target: %v", err)
	}
	slots := newRecordingReadSlots()

	reader, limiter, err := readerFactoryFor(omode.CatClient)(fileReaderFactoryOptions(slots, &target))
	if err == nil {
		t.Fatalf("factory() = (%T, %v, nil), want journal construction error", reader, limiter)
	}
	if reader != nil || limiter != nil {
		t.Fatalf("factory() = (%T, %v, %v), want nil reader and limiter", reader, limiter, err)
	}
	if slots.calls != 0 {
		t.Fatalf("AcquireReadSlot() calls = %d, want 0", slots.calls)
	}
}

func TestReadCommandJournalConstructionFailureWarnsAndSkipsLimiter(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	target, err := fs.NewValidatedJournalTarget("journal:ssh.service")
	if err != nil {
		t.Fatalf("create journal target: %v", err)
	}
	server := newJournalReadTestServer()
	command := newReadCommand(server, omode.CatClient)
	command.logger = &warningTextLogger{}

	command.read(context.Background(), emptyLContext(), "journal:ssh.service",
		&target, "journal:ssh.service", regex.NewNoop())

	if got := len(server.catLimiter); got != 0 {
		t.Fatalf("cat limiter occupancy = %d, want 0", got)
	}
	select {
	case encoded := <-server.serverMessage:
		_, message := decodeGeneratedMessage(encoded)
		if !strings.Contains(message, "Unable to read journal") {
			t.Fatalf("server message = %q, want journal warning", message)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for journal construction warning")
	}
}

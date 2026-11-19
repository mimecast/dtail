package aggregate

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

// waitForGroups polls the aggregate until its groups equal want, and fails the
// test with the last groups seen when they do not within the timeout.
func waitForGroups(t *testing.T, aggregate *Aggregate, want map[string]int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := groupSamples(aggregate)
		if maps.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("groups = %v, want %v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCSVFollowReaderInPlaceTruncationInstallsNewHeader drives a real
// follow-mode ReadFile into an aggregate Processor through an in-place
// (copytruncate style) truncation. The reader rewinds the same descriptor and
// keeps feeding the same Processor, so the content written after the
// truncation must get a CSV header of its own: its first line is a header,
// and here its column order differs from the old content's. Parsed against
// the stale header, the new header row and every new data row were mapped
// with the old column order (groups "value", "3" and "4" instead of "z").
func TestCSVFollowReaderInPlaceTruncationInstallsNewHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.csv")
	// The old content is longer than the new one, so the reader detects the
	// truncation by its read offset exceeding the file size, whether it
	// checks before or after the new content was written.
	if err := os.WriteFile(path, []byte("label,value\na,11\na,22\n"), 0o600); err != nil {
		t.Fatalf("write old content: %v", err)
	}

	reader, err := fs.NewReadFile(fs.ReadOptions{
		Mode:     omode.TailClient,
		FilePath: path,
		GlobID:   "x.csv",
		Logger:   logging.NopLogger{},
	})
	if err != nil {
		t.Fatalf("NewReadFile() error = %v", err)
	}

	aggregate := newCSVSourceTestAggregate(t)
	processor := NewProcessor(aggregate, "x.csv")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- reader.Start(ctx, lcontext.LContext{}, processor, regex.NewNoop())
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case startErr := <-done:
			if startErr != nil {
				t.Errorf("Start() error = %v", startErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("follow reader did not stop after cancellation")
		}
		closeProcessor(t, processor)
	}
	defer stop()

	waitForGroups(t, aggregate, map[string]int{"a": 2})

	if truncErr := os.Truncate(path, 0); truncErr != nil {
		t.Fatalf("truncate: %v", truncErr)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := file.WriteString("value,label\n3,z\n4,z\n"); err != nil {
		_ = file.Close()
		t.Fatalf("append new content: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close appended file: %v", err)
	}

	want := map[string]int{"a": 2, "z": 2}
	waitForGroups(t, aggregate, want)
	stop()
	if got := groupSamples(aggregate); !maps.Equal(got, want) {
		t.Errorf("groups after close = %v, want %v", got, want)
	}
	if got := aggregate.errors.Load(); got != 0 {
		t.Errorf("errors = %d, want 0", got)
	}
}

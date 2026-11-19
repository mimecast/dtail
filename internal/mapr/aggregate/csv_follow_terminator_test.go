package aggregate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/fs/readhub"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

func TestCSVFollowCRLFRecords(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, before := range []int{0, 1} {
			t.Run(fmt.Sprintf("shared=%v/before=%d", shared, before), func(t *testing.T) {
				testCSVFollowCRLFRecords(t, shared, before)
			})
		}
	}
}

func testCSVFollowCRLFRecords(t *testing.T, shared bool, before int) {
	path := filepath.Join(t.TempDir(), "follow.csv")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	aggregate := newCSVSourceTestAggregate(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ltx := lcontext.LContext{BeforeContext: before}
	if shared {
		target, err := fs.NewValidatedReadTarget(path)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		hub := readhub.New(readhub.Options{Logger: logging.NopLogger{}})
		go func() {
			done <- hub.Follow(ctx, readhub.Session{
				Target: target, FilePath: path, GlobID: "follow.csv",
				LContext: ltx, Regex: regex.NewNoop(),
				NewProcessor: func() line.Processor { return NewProcessor(aggregate, "follow.csv") },
			})
		}()
		defer stopCSVFollow(t, cancel, done)
		deadline := time.Now().Add(5 * time.Second)
		for hub.Subscribers(path) != 1 {
			if time.Now().After(deadline) {
				t.Fatal("shared reader did not subscribe")
			}
			time.Sleep(time.Millisecond)
		}
	} else {
		reader, err := fs.NewReadFile(fs.ReadOptions{Mode: omode.TailClient, FilePath: path, GlobID: "follow.csv", Logger: logging.NopLogger{}})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		processor := NewProcessor(aggregate, "follow.csv")
		defer closeProcessor(t, processor)
		go func() { done <- reader.Start(ctx, ltx, processor, regex.NewNoop()) }()
		defer stopCSVFollow(t, cancel, done)
	}
	appendCSVFollow(t, path, "label\r\nJane \r\n\"Smith, Jane\"\r\n")
	waitForGroups(t, aggregate, map[string]int{"Jane ": 1, "Smith, Jane": 1})
	// The next record arrives after the reader has reached EOF and resumes.
	appendCSVFollow(t, path, "\"JJ \"\"quoted\"\"\"\r\nJane\r\n")
	waitForGroups(t, aggregate, map[string]int{"Jane ": 1, "Smith, Jane": 1, `JJ "quoted"`: 1, "Jane": 1})
	if got := aggregate.errors.Load(); got != 0 {
		t.Fatalf("parse errors = %d", got)
	}
}

func appendCSVFollow(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if _, writeErr := f.WriteString(content); writeErr != nil {
		t.Fatal(writeErr)
	}
}

func stopCSVFollow(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("follow: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("follow reader did not stop")
	}
}

package handlers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
)

type recordingReadSlots struct {
	catLimiter  chan struct{}
	tailLimiter chan struct{}
	mode        omode.Mode
	path        string
	calls       int
}

func newRecordingReadSlots() *recordingReadSlots {
	return &recordingReadSlots{
		catLimiter:  make(chan struct{}, 1),
		tailLimiter: make(chan struct{}, 1),
	}
}

func (s *recordingReadSlots) AcquireReadSlot(_ context.Context, mode omode.Mode,
	path string) (func(), bool) {

	s.mode = mode
	s.path = path
	s.calls++
	limiter := s.tailLimiter
	if mode == omode.CatClient || mode == omode.GrepClient {
		limiter = s.catLimiter
	}
	limiter <- struct{}{}
	return func() { <-limiter }, true
}

func TestReaderFactorySelectsModeBehaviorAndLimiter(t *testing.T) {
	tests := []struct {
		name         string
		mode         omode.Mode
		wantMode     omode.Mode
		wantRetry    bool
		wantCatSlots int
		wantTailSlot int
	}{
		{name: "cat", mode: omode.CatClient, wantMode: omode.CatClient, wantCatSlots: 1},
		{name: "grep", mode: omode.GrepClient, wantMode: omode.GrepClient, wantCatSlots: 1},
		{name: "tail", mode: omode.TailClient, wantMode: omode.TailClient, wantRetry: true, wantTailSlot: 1},
		{
			name:         "unknown preserves tail fallback",
			mode:         omode.Unknown,
			wantMode:     omode.TailClient,
			wantRetry:    true,
			wantTailSlot: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slots := newRecordingReadSlots()
			factory := readerFactoryFor(tt.mode)
			reader, limiter, err := factory(fileReaderFactoryOptions(slots, nil))
			if err != nil {
				t.Fatalf("factory() error = %v", err)
			}
			if _, ok := reader.(*fs.ReadFile); !ok {
				t.Fatalf("reader type = %T, want *fs.ReadFile", reader)
			}
			if got := reader.Retry(); got != tt.wantRetry {
				t.Errorf("Retry() = %v, want %v", got, tt.wantRetry)
			}

			release, acquired := limiter(context.Background(), "application.log")
			if !acquired {
				t.Fatal("limiter was not acquired")
			}
			if slots.mode != tt.wantMode {
				t.Errorf("limiter mode = %v, want %v", slots.mode, tt.wantMode)
			}
			if slots.path != "application.log" {
				t.Errorf("limiter path = %q, want application.log", slots.path)
			}
			if got := len(slots.catLimiter); got != tt.wantCatSlots {
				t.Errorf("cat limiter occupancy = %d, want %d", got, tt.wantCatSlots)
			}
			if got := len(slots.tailLimiter); got != tt.wantTailSlot {
				t.Errorf("tail limiter occupancy = %d, want %d", got, tt.wantTailSlot)
			}
			release()
			if len(slots.catLimiter) != 0 || len(slots.tailLimiter) != 0 {
				t.Fatal("limiter release left an occupied slot")
			}
		})
	}
}

func TestReaderFactoryRejectsInvalidInputsBeforeLimiterAcquisition(t *testing.T) {
	tests := []struct {
		name    string
		options func(*recordingReadSlots) readerFactoryOptions
	}{
		{
			name: "missing slot acquirer",
			options: func(*recordingReadSlots) readerFactoryOptions {
				return fileReaderFactoryOptions(nil, nil)
			},
		},
		{
			name: "invalid file target kind",
			options: func(slots *recordingReadSlots) readerFactoryOptions {
				target := fs.ValidatedReadTarget{Kind: fs.ReadTargetKind(255)}
				return fileReaderFactoryOptions(slots, &target)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slots := newRecordingReadSlots()
			reader, limiter, err := readerFactoryFor(omode.CatClient)(tt.options(slots))
			if err == nil {
				t.Fatalf("factory() = (%T, %v, nil), want error", reader, limiter)
			}
			if reader != nil || limiter != nil {
				t.Fatalf("factory() = (%T, %v, %v), want nil reader and limiter", reader, limiter, err)
			}
			if slots.calls != 0 {
				t.Fatalf("AcquireReadSlot() calls = %d, want 0", slots.calls)
			}
		})
	}
}

func TestReaderFactoryCreatesValidatedFileReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "application.log")
	if err := os.WriteFile(path, []byte("alpha\n"), 0600); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	target, err := fs.NewValidatedReadTarget(path)
	if err != nil {
		t.Fatalf("create validated target: %v", err)
	}

	reader, limiter, err := readerFactoryFor(omode.CatClient)(
		fileReaderFactoryOptions(newRecordingReadSlots(), &target),
	)
	if err != nil {
		t.Fatalf("factory() error = %v", err)
	}
	if _, ok := reader.(*fs.ReadFile); !ok {
		t.Fatalf("reader type = %T, want *fs.ReadFile", reader)
	}
	if limiter == nil {
		t.Fatal("factory returned nil limiter")
	}
}

func fileReaderFactoryOptions(slots readSlotAcquirer, target *fs.ValidatedReadTarget) readerFactoryOptions {
	return readerFactoryOptions{
		slots:          slots,
		target:         target,
		path:           "application.log",
		globID:         "application.log",
		serverMessages: make(chan string, 1),
		maxLineLength:  1024,
		logger:         logging.NopLogger{},
	}
}

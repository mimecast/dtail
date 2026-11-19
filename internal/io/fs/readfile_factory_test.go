package fs

import (
	"context"
	"reflect"
	"testing"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

func TestNewReadFileConfiguresModeBehavior(t *testing.T) {
	tests := []struct {
		name        string
		mode        omode.Mode
		seekEOF     bool
		wantRetry   bool
		wantFollow  bool
		wantCanSkip bool
	}{
		{name: "cat snapshot", mode: omode.CatClient},
		{name: "grep snapshot", mode: omode.GrepClient},
		{
			name:        "tail follow",
			mode:        omode.TailClient,
			seekEOF:     true,
			wantRetry:   true,
			wantFollow:  true,
			wantCanSkip: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, err := NewReadFile(ReadOptions{
				Mode:     tt.mode,
				FilePath: "application.log",
				GlobID:   "application.log",
				SeekEOF:  tt.seekEOF,
			})
			if err != nil {
				t.Fatalf("NewReadFile() error = %v", err)
			}
			if got := reader.Retry(); got != tt.wantRetry {
				t.Errorf("Retry() = %v, want %v", got, tt.wantRetry)
			}
			if reader.follow != tt.wantFollow {
				t.Errorf("follow = %v, want %v", reader.follow, tt.wantFollow)
			}
			if reader.canSkipLines != tt.wantCanSkip {
				t.Errorf("canSkipLines = %v, want %v", reader.canSkipLines, tt.wantCanSkip)
			}
			if reader.seekInitialEOF != tt.seekEOF {
				t.Errorf("seekInitialEOF = %v, want %v", reader.seekInitialEOF, tt.seekEOF)
			}
		})
	}
}

func TestNewReadFileRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name    string
		options ReadOptions
	}{
		{
			name:    "unsupported mode",
			options: ReadOptions{Mode: omode.Unknown},
		},
		{
			name: "journal target",
			options: ReadOptions{
				Mode:   omode.CatClient,
				Target: &ValidatedReadTarget{Kind: JournalKind},
			},
		},
		{
			name: "unknown target kind",
			options: ReadOptions{
				Mode:   omode.CatClient,
				Target: &ValidatedReadTarget{Kind: ReadTargetKind(255)},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if reader, err := NewReadFile(tt.options); err == nil {
				t.Fatalf("NewReadFile() = %#v, want error", reader)
			}
		})
	}
}

func TestNewReadFileCopiesValidatedTarget(t *testing.T) {
	filePath := writeProcessorTestFile(t, "alpha\n")
	target := mustValidatedReadTarget(t, filePath)
	reader, err := NewReadFile(ReadOptions{
		Mode:     omode.CatClient,
		Target:   &target,
		FilePath: filePath,
		GlobID:   "glob-id",
		Logger:   testLogger,
	})
	if err != nil {
		t.Fatalf("NewReadFile() error = %v", err)
	}

	target.Kind = JournalKind
	processor := &captureProcessor{}
	if err := reader.Start(context.Background(), lcontext.LContext{}, processor, regex.NewNoop()); err != nil {
		t.Fatalf("Start() error after caller mutated target = %v", err)
	}
	if want := []string{"alpha\n"}; !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("processed lines = %q, want %q", processor.lines, want)
	}
}

func newSnapshotReadFile(filePath, globID string, serverMessages chan<- string,
	maxLineLength int, logger logging.Logger) *ReadFile {

	return mustNewReadFile(ReadOptions{
		Mode:           omode.CatClient,
		FilePath:       filePath,
		GlobID:         globID,
		ServerMessages: serverMessages,
		MaxLineLength:  maxLineLength,
		Logger:         logger,
	})
}

func newFollowReadFile(filePath, globID string, serverMessages chan<- string,
	maxLineLength int, logger logging.Logger) *ReadFile {

	return mustNewReadFile(ReadOptions{
		Mode:           omode.TailClient,
		FilePath:       filePath,
		GlobID:         globID,
		ServerMessages: serverMessages,
		SeekEOF:        true,
		MaxLineLength:  maxLineLength,
		Logger:         logger,
	})
}

func mustNewReadFile(options ReadOptions) *ReadFile {
	reader, err := NewReadFile(options)
	if err != nil {
		panic(err)
	}
	return reader
}

package fs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

func TestValidatedCatFileStartReadsAllLines(t *testing.T) {
	filePath := writeProcessorTestFile(t, "alpha\nbeta\n")
	target := mustValidatedReadTarget(t, filePath)
	re := regex.NewNoop()

	cat := mustNewReadFile(ReadOptions{
		Mode:           omode.CatClient,
		Target:         &target,
		FilePath:       filePath,
		GlobID:         "glob-id",
		ServerMessages: make(chan string, 1),
		MaxLineLength:  defaultMaxLineLength,
		Logger:         testLogger,
	})
	processor := &captureProcessor{}

	if err := cat.Start(
		context.Background(),
		lcontext.LContext{},
		processor,
		re,
	); err != nil {
		t.Fatalf("validated optimized reader start failed: %v", err)
	}

	want := []string{"alpha\n", "beta\n"}
	if !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("unexpected processed lines: got=%v want=%v", processor.lines, want)
	}
}

func TestValidatedReadTargetOpenRejectsEscapingSymlinkSwap(t *testing.T) {
	baseDir := t.TempDir()
	rootDir := filepath.Join(baseDir, "root")
	outsideDir := filepath.Join(baseDir, "outside")
	if err := os.MkdirAll(rootDir, 0755); err != nil {
		t.Fatalf("mkdir root dir: %v", err)
	}
	if err := os.MkdirAll(outsideDir, 0755); err != nil {
		t.Fatalf("mkdir outside dir: %v", err)
	}

	filePath := filepath.Join(rootDir, "app.log")
	if err := os.WriteFile(filePath, []byte("alpha\n"), 0600); err != nil {
		t.Fatalf("write app log: %v", err)
	}
	escapePath := filepath.Join(outsideDir, "secret.log")
	if err := os.WriteFile(escapePath, []byte("secret\n"), 0600); err != nil {
		t.Fatalf("write secret log: %v", err)
	}

	target := mustValidatedReadTarget(t, filePath)

	if err := os.Remove(filePath); err != nil {
		t.Fatalf("remove app log: %v", err)
	}
	relativeEscape, err := filepath.Rel(rootDir, escapePath)
	if err != nil {
		t.Fatalf("relative escape path: %v", err)
	}
	if err := os.Symlink(relativeEscape, filePath); err != nil {
		t.Fatalf("symlink escape path: %v", err)
	}

	if _, err := target.Open(); err == nil {
		t.Fatal("expected rooted open to reject escaping symlink swap")
	}
}

func TestValidatedReadTargetOpenRejectsSameRootSymlinkSwap(t *testing.T) {
	baseDir := t.TempDir()
	filePath := filepath.Join(baseDir, "app.log")
	if err := os.WriteFile(filePath, []byte("alpha\n"), 0600); err != nil {
		t.Fatalf("write app log: %v", err)
	}
	otherPath := filepath.Join(baseDir, "other.log")
	if err := os.WriteFile(otherPath, []byte("other\n"), 0600); err != nil {
		t.Fatalf("write other log: %v", err)
	}

	target := mustValidatedReadTarget(t, filePath)

	if err := os.Remove(filePath); err != nil {
		t.Fatalf("remove app log: %v", err)
	}
	if err := os.Symlink(filepath.Base(otherPath), filePath); err != nil {
		t.Fatalf("symlink other log: %v", err)
	}

	_, err := target.Open()
	if err == nil {
		t.Fatal("expected rooted open to reject same-root symlink swap")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestNewValidatedJournalTargetRejectsAmbiguousUnits(t *testing.T) {
	tests := []struct {
		name string
		spec string
	}{
		{
			name: "empty unit",
			spec: "journal:",
		},
		{
			name: "leading dash",
			spec: "journal:--output=json",
		},
		{
			name: "space",
			spec: "journal:ssh.service --output=json",
		},
		{
			name: "newline",
			spec: "journal:ssh.service\n--output=json",
		},
		{
			name: "tab",
			spec: "journal:ssh.service\t--output=json",
		},
		{
			name: "control character",
			spec: "journal:ssh.service\x00",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewValidatedJournalTarget(tt.spec); err == nil {
				t.Fatalf("NewValidatedJournalTarget(%q) succeeded, want error", tt.spec)
			}
		})
	}
}

func TestNewValidatedJournalTargetAcceptsLiteralShellMetacharacters(t *testing.T) {
	spec := "journal:ssh.service;touch$HOME`whoami`|cat"

	target, err := NewValidatedJournalTarget(spec)
	if err != nil {
		t.Fatalf("NewValidatedJournalTarget(%q) error = %v", spec, err)
	}
	if target.Kind != JournalKind {
		t.Fatalf("target.Kind = %v, want %v", target.Kind, JournalKind)
	}
}

func TestValidatedTailFileTruncatedReopenDetectsTruncation(t *testing.T) {
	filePath := writeProcessorTestFile(t, "alpha\nbeta\n")
	target := mustValidatedReadTarget(t, filePath)

	tail := mustNewReadFile(ReadOptions{
		Mode:           omode.TailClient,
		Target:         &target,
		FilePath:       filePath,
		GlobID:         "glob-id",
		ServerMessages: make(chan string, 1),
		SeekEOF:        true,
		MaxLineLength:  defaultMaxLineLength,
		Logger:         testLogger,
	})
	fd, openErr := target.Open()
	if openErr != nil {
		t.Fatalf("open validated target: %v", openErr)
	}
	t.Cleanup(func() {
		if closeErr := fd.Close(); closeErr != nil {
			t.Errorf("close validated target: %v", closeErr)
		}
	})

	if _, err := fd.Seek(0, io.SeekEnd); err != nil {
		t.Fatalf("seek end: %v", err)
	}
	if err := os.Truncate(filePath, 1); err != nil {
		t.Fatalf("truncate file: %v", err)
	}

	isTruncated, err := tail.truncated(fd)
	if !isTruncated {
		t.Fatal("expected truncation to be detected")
	}
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("expected truncation error, got %v", err)
	}
}

func mustValidatedReadTarget(t *testing.T, path string) ValidatedReadTarget {
	t.Helper()

	absolutePath, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	target, err := NewValidatedReadTarget(absolutePath)
	if err != nil {
		t.Fatalf("create validated target: %v", err)
	}

	return target
}

package fs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

func writeStartOffsetTestFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func statStartOffsetTestFile(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestNewReadFileValidatesStartOffset(t *testing.T) {
	identity := statStartOffsetTestFile(t, writeStartOffsetTestFile(t, "id.log", "x\n"))
	tests := []struct {
		name    string
		options ReadOptions
		wantErr string
	}{
		{"zero offset", ReadOptions{FilePath: "/tmp/a.log"}, ""},
		{"positive offset", ReadOptions{FilePath: "/tmp/a.log", StartOffset: 10, StartOffsetFile: identity}, ""},
		{"offset without file identity", ReadOptions{FilePath: "/tmp/a.log", StartOffset: 10}, "identity"},
		{"zero offset with seek EOF", ReadOptions{FilePath: "/tmp/a.log", SeekEOF: true}, ""},
		{"zero offset on a compressed file", ReadOptions{FilePath: "/tmp/a.log.gz"}, ""},
		{"negative offset", ReadOptions{FilePath: "/tmp/a.log", StartOffset: -1}, "negative"},
		{"start file without offset", ReadOptions{FilePath: "/tmp/a.log", StartFile: os.Stdin}, ""},
		{"start file with seek EOF", ReadOptions{FilePath: "/tmp/a.log", StartFile: os.Stdin, SeekEOF: true},
			"mutually exclusive"},
		{"start file on the stdin pipe", ReadOptions{GlobID: "-", StartFile: os.Stdin}, "stdin pipe"},
		{"start file on a zst file", ReadOptions{FilePath: "/tmp/a.log.zst", StartFile: os.Stdin}, "compressed"},
		{"offset with seek EOF", ReadOptions{FilePath: "/tmp/a.log", StartOffset: 10, SeekEOF: true, StartOffsetFile: identity},
			"mutually exclusive"},
		{"offset on the stdin pipe", ReadOptions{GlobID: "-", StartOffset: 10, StartOffsetFile: identity}, "stdin pipe"},
		{"offset on a gz file", ReadOptions{FilePath: "/tmp/a.log.gz", StartOffset: 10, StartOffsetFile: identity}, "compressed"},
		{"offset on a gzip file", ReadOptions{FilePath: "/tmp/a.log.gzip", StartOffset: 10, StartOffsetFile: identity}, "compressed"},
		{"offset on a zst file", ReadOptions{FilePath: "/tmp/a.log.zst", StartOffset: 10, StartOffsetFile: identity}, "compressed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := tt.options
			options.Mode = omode.TailClient
			reader, err := NewReadFile(options)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("NewReadFile() error = %v, want nil", err)
				}
				if reader.initialOffset != tt.options.StartOffset {
					t.Fatalf("initialOffset = %d, want %d", reader.initialOffset, tt.options.StartOffset)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("NewReadFile() error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestStartOffsetSnapshotReadStartsAtOffset(t *testing.T) {
	path := writeStartOffsetTestFile(t, "offset.log", "alpha\nbeta\ngamma\n")
	reader := mustNewReadFile(ReadOptions{
		Mode:            omode.CatClient,
		FilePath:        path,
		GlobID:          "glob",
		StartOffset:     int64(len("alpha\n")),
		StartOffsetFile: statStartOffsetTestFile(t, path),
		Logger:          testLogger,
	})
	processor := &captureProcessor{}
	if err := reader.Start(context.Background(), lcontext.LContext{}, processor, regex.NewNoop()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// Line numbers count from the offset, as they count from EOF for SeekEOF.
	if want := []string{"beta\n", "gamma\n"}; !reflect.DeepEqual(processor.lines, want) {
		t.Errorf("lines = %q, want %q", processor.lines, want)
	}
	if want := []uint64{1, 2}; !reflect.DeepEqual(processor.lineNums, want) {
		t.Errorf("line numbers = %v, want %v", processor.lineNums, want)
	}
}

func readAllFromNewReader(t *testing.T, reader *ReadFile) string {
	t.Helper()
	bufReader, fd, decompressor, err := reader.makeFileReader()
	if err != nil {
		t.Fatalf("makeFileReader() error = %v", err)
	}
	defer func() { _ = fd.Close() }()
	if decompressor != nil {
		defer func() { _ = decompressor.Close() }()
	}
	data, err := io.ReadAll(bufReader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(data)
}

func TestStartOffsetAppliesToFirstSuccessfulOpenOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "late.log")
	staged := writeStartOffsetTestFile(t, "staged.log", "alpha\nbeta\n")
	reader := mustNewReadFile(ReadOptions{
		Mode:            omode.TailClient,
		FilePath:        path,
		GlobID:          "glob",
		StartOffset:     int64(len("alpha\n")),
		StartOffsetFile: statStartOffsetTestFile(t, staged),
		Logger:          testLogger,
	})

	// A failed first open must keep the offset for the retry.
	if _, fd, _, err := reader.makeFileReader(); err == nil {
		_ = fd.Close()
		t.Fatal("makeFileReader() on a missing file succeeded")
	}
	if reader.initialOffset == 0 {
		t.Fatal("a failed open cleared the start offset")
	}

	// The file the offset was measured in appears at the path.
	if err := os.Rename(staged, path); err != nil {
		t.Fatal(err)
	}
	if got := readAllFromNewReader(t, reader); got != "beta\n" {
		t.Errorf("first successful open read %q, want %q", got, "beta\n")
	}
	// A reopen, e.g. after rotation, starts at the beginning as usual.
	if got := readAllFromNewReader(t, reader); got != "alpha\nbeta\n" {
		t.Errorf("reopen read %q, want %q", got, "alpha\nbeta\n")
	}
}

// cancelOnLineProcessor records lines and cancels the read once it has want.
type cancelOnLineProcessor struct {
	captureProcessor
	want   int
	cancel context.CancelFunc
}

func (p *cancelOnLineProcessor) ProcessLine(lineContent *bytes.Buffer, lineNum uint64, sourceID string) error {
	err := p.captureProcessor.ProcessLine(lineContent, lineNum, sourceID)
	if len(p.lines) >= p.want {
		p.cancel()
	}
	return err
}

func TestStartOffsetBeyondFileSizeRewindsFollowRead(t *testing.T) {
	path := writeStartOffsetTestFile(t, "short.log", "alpha\nbeta\n")
	reader := mustNewReadFile(ReadOptions{
		Mode:            omode.TailClient,
		FilePath:        path,
		GlobID:          "glob",
		StartOffset:     1000,
		StartOffsetFile: statStartOffsetTestFile(t, path),
		Logger:          testLogger,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	processor := &cancelOnLineProcessor{want: 2, cancel: cancel}
	if err := reader.Start(ctx, lcontext.LContext{}, processor, regex.NewNoop()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// The file is shorter than the offset, so the follow reader treats it as
	// truncated and reads it from the beginning.
	if want := []string{"alpha", "beta"}; !reflect.DeepEqual(processor.lines, want) {
		t.Errorf("lines = %q, want %q (context error %v)", processor.lines, want, ctx.Err())
	}
}

func TestStartOffsetIsIgnoredForAnotherFile(t *testing.T) {
	// The offset was taken in the old file, which was then rotated away and
	// replaced by a new file at the same path.
	path := writeStartOffsetTestFile(t, "rotated.log", "old1\nold2\n")
	oldFile := statStartOffsetTestFile(t, path)
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new1\nnew2\nnew3\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	reader := mustNewReadFile(ReadOptions{
		Mode:            omode.CatClient,
		FilePath:        path,
		GlobID:          "glob",
		StartOffset:     int64(len("old1\nold2\n")),
		StartOffsetFile: oldFile,
		Logger:          testLogger,
	})
	processor := &captureProcessor{}
	// The reader reports the rotation before reading anything, so that the
	// caller can prepare for the new file.
	err := reader.Start(context.Background(), lcontext.LContext{}, processor, regex.NewNoop())
	if !errors.Is(err, ErrStartOffsetFileChanged) {
		t.Fatalf("Start() error = %v, want ErrStartOffsetFileChanged", err)
	}
	if len(processor.lines) != 0 {
		t.Fatalf("Start() read %q from the new file before reporting the rotation", processor.lines)
	}

	// The next start reads the new file from its beginning; no line of it is
	// lost.
	if err := reader.Start(context.Background(), lcontext.LContext{}, processor, regex.NewNoop()); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	if want := []string{"new1\n", "new2\n", "new3\n"}; !reflect.DeepEqual(processor.lines, want) {
		t.Errorf("lines = %q, want %q", processor.lines, want)
	}
}

func TestStartOffsetInSplitLineDoesNotWarnAgain(t *testing.T) {
	const maxLineLength = 8
	// An unfinished line longer than the limit: an earlier reader split it
	// after its first maxLineLength bytes.
	long := strings.Repeat("a", maxLineLength) + strings.Repeat("b", 2*maxLineLength)
	for _, inSplitLine := range []bool{false, true} {
		t.Run(fmt.Sprintf("inSplitLine=%v", inSplitLine), func(t *testing.T) {
			path := writeStartOffsetTestFile(t, "split.log", long)
			messages := make(chan string, 10)
			reader := mustNewReadFile(ReadOptions{
				Mode:                   omode.TailClient,
				FilePath:               path,
				GlobID:                 "glob",
				StartOffset:            maxLineLength,
				StartOffsetFile:        statStartOffsetTestFile(t, path),
				StartOffsetInSplitLine: inSplitLine,
				ServerMessages:         messages,
				MaxLineLength:          maxLineLength,
				Logger:                 testLogger,
			})
			processor := &positionProcessor{}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- reader.Start(ctx, lcontext.LContext{}, processor, regex.NewNoop()) }()
			rest := strings.Repeat("b", 2*maxLineLength)
			waitUntil(t, "the rest of the long line", func() bool {
				lines, _, _ := processor.state()
				return len(lines) > 0 && lines[0] == rest
			})
			appendToFile(t, path, "\nshort\n")
			waitUntil(t, "the next line", func() bool {
				lines, _, _ := processor.state()
				return len(lines) == 2 && lines[1] == "short"
			})
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			// The rest of the line is still longer than the limit: a reader
			// that starts in it warns only if nobody warned about it before.
			want := 1
			if inSplitLine {
				want = 0
			}
			if got := len(messages); got != want {
				t.Errorf("sent %d long line warnings, want %d", got, want)
			}
		})
	}
}

func TestStartFileIsReadEvenAfterARotation(t *testing.T) {
	path := writeStartOffsetTestFile(t, "held.log", "old1\nold2\n")
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := held.Stat()
	if err != nil {
		t.Fatal(err)
	}
	// The path is rotated after the descriptor was opened, and the old file
	// still grows.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte("old1\nold2\nold3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := mustNewReadFile(ReadOptions{
		Mode:            omode.CatClient,
		FilePath:        path,
		GlobID:          "glob",
		StartOffset:     int64(len("old1\n")),
		StartOffsetFile: info,
		StartFile:       held,
		Logger:          testLogger,
	})
	processor := &captureProcessor{}
	if err := reader.Start(context.Background(), lcontext.LContext{}, processor, regex.NewNoop()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if want := []string{"old2\n", "old3\n"}; !reflect.DeepEqual(processor.lines, want) {
		t.Errorf("lines = %q, want the old file's lines after the offset %q", processor.lines, want)
	}
	if err := held.Close(); err == nil {
		t.Error("the reader did not close the start file")
	}
	// The next read opens the path.
	processor.lines = nil
	if err := reader.Start(context.Background(), lcontext.LContext{}, processor, regex.NewNoop()); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	if want := []string{"new1\n"}; !reflect.DeepEqual(processor.lines, want) {
		t.Errorf("second read lines = %q, want %q", processor.lines, want)
	}
}

func TestStartFileWithoutOffsetIsReadFromItsBeginning(t *testing.T) {
	path := writeStartOffsetTestFile(t, "held0.log", "old1\nold2\n")
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := mustNewReadFile(ReadOptions{
		Mode:      omode.CatClient,
		FilePath:  path,
		GlobID:    "glob",
		StartFile: held,
		Logger:    testLogger,
	})
	processor := &captureProcessor{}
	if err := reader.Start(context.Background(), lcontext.LContext{}, processor, regex.NewNoop()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if want := []string{"old1\n", "old2\n"}; !reflect.DeepEqual(processor.lines, want) {
		t.Errorf("lines = %q, want the whole held file %q", processor.lines, want)
	}
	if err := held.Close(); err == nil {
		t.Error("the reader did not close the start file")
	}
}

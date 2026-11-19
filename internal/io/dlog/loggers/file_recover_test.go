package loggers

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, syscall.ENOSPC }

// One failed write must not make the logger discard every later line: the
// bufio.Writer's sticky error is cleared and writes go to the file again.
func TestFileLoggerRecoversAfterWriteError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dserver.log")
	fd, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f := newFile(Strategy{Rotation: SignalRotation, FileBase: "dserver"}, dir)
	f.fd = fd
	f.writer = bufio.NewWriterSize(failingWriter{}, fileWriterBufSize)
	f.lastFileName = "dserver"

	if err := f.write([]byte("lost\n")); err != nil {
		t.Fatalf("buffered write: %v", err)
	}
	if err := f.flushWriter(); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("expected ENOSPC from the flush, got %v", err)
	}
	if err := f.write([]byte("kept\n")); err != nil {
		t.Fatalf("write after the failure: %v", err)
	}
	if err := f.flushWriter(); err != nil {
		t.Fatalf("flush after the failure: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "kept\n") {
		t.Fatalf("expected later line in the log file, got %q", got)
	}
}

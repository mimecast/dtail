package fs

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

func gzipMember(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A follow read of a .gz file starts at its end, where no gzip header is to
// be read yet. It must wait there and then deliver the lines of the gzip
// members appended later, instead of failing with io.EOF for good.
func TestFollowReadOfGzipFileReadsAppendedMembers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log.gz")
	if err := os.WriteFile(path, gzipMember(t, "old 1\nold 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	reader := newFollowReadFile(path, "glob", nil, defaultMaxLineLength, testLogger)
	p := &captureProcessor{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- reader.Start(ctx, lcontext.LContext{}, p, regex.NewNoop()) }()

	appendMember := func(content string) {
		fd, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = fd.Close() }()
		if _, err := fd.Write(gzipMember(t, content)); err != nil {
			t.Error(err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	appendMember("new 1\n")
	time.Sleep(700 * time.Millisecond)
	appendMember("new 2\nnew 3\n")
	time.Sleep(700 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("follow read of a gzip file failed: %v", err)
	}

	var got []string
	for _, l := range p.lines {
		got = append(got, strings.TrimSuffix(l, "\n"))
	}
	if want := []string{"new 1", "new 2", "new 3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

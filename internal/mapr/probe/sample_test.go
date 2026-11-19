package probe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mimecast/dtail/internal/io/fs"
)

func TestSampleFileTarget(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "app.log")
	if err = os.WriteFile(path, []byte("header\nfirst\nsecond\nthird\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := fs.NewValidatedReadTarget(path)
	if err != nil {
		t.Fatal(err)
	}

	got, err := Sample(context.Background(), target, path, fs.SampleOptions{WindowBytes: 13})
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	want := fs.Sample{Lines: []string{"second", "third"}, FirstLine: "header", HasFirstLine: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sample = %#v, want %#v", got, want)
	}
}

func TestSampleCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target, err := fs.NewValidatedJournalTarget("journal:ssh.service")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Sample(ctx, target, "journal:ssh.service", fs.SampleOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

//go:build !nozstd

package fs

import (
	"context"
	"testing"

	"github.com/DataDog/zstd"
)

// TestSampleFileZstdHead samples a zstd file at its beginning, like a gzip
// file.
func TestSampleFileZstdHead(t *testing.T) {
	compressed, err := zstd.Compress(nil, []byte(numberedLines(1, 20)))
	if err != nil {
		t.Fatal(err)
	}
	path, target := writeSampleFile(t, "sample.log.zst", compressed)
	got, err := SampleFile(context.Background(), target, path, SampleOptions{MaxLines: 3})
	if err != nil {
		t.Fatalf("SampleFile: %v", err)
	}
	assertSample(t, got, Sample{Lines: numberedSample(1, 3)})
}

func TestSampleFileCorruptZstd(t *testing.T) {
	path, target := writeSampleFile(t, "corrupt.log.zst", []byte("not zstd\n"))
	if _, err := SampleFile(context.Background(), target, path, SampleOptions{}); err == nil {
		t.Fatal("expected an error for a corrupt zstd file")
	}
}

//go:build nozstd

package fs

import (
	"context"
	"strings"
	"testing"
)

// TestSampleFileZstdUnsupported checks that a build without zstd reports an
// error for a .zst file instead of sampling its compressed bytes.
func TestSampleFileZstdUnsupported(t *testing.T) {
	path, target := writeSampleFile(t, "sample.log.zst", []byte("anything\n"))
	_, err := SampleFile(context.Background(), target, path, SampleOptions{})
	if err == nil || !strings.Contains(err.Error(), "zstd is not supported") {
		t.Fatalf("err = %v, want the zstd unsupported error", err)
	}
}

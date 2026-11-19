//go:build !nozstd

package fs

import (
	"testing"

	"github.com/DataDog/zstd"
)

// TestStartReadsLongLinesOfZstdFiles runs the long-line cases through the
// zstd decompressor (not built with -tags nozstd), whose reader hands the
// scanner data in other chunks.
func TestStartReadsLongLinesOfZstdFiles(t *testing.T) {
	for _, tt := range longLineCases() {
		t.Run(tt.name, func(t *testing.T) {
			compressed, err := zstd.Compress(nil, []byte(tt.content))
			if err != nil {
				t.Fatal(err)
			}
			assertLongLineRead(t, writeLongLineFile(t, "test.log.zst", compressed), tt)
		})
	}
}

//go:build nozstd

package fs

import (
	"bufio"
	"fmt"
	"io"
	"os"
)

func (f *ReadFile) makeZstdReader(fd *os.File) (reader *bufio.Reader, decompressor io.Closer, err error) {
	_, err = newZstdDecompressor(f.FilePath(), fd)
	return
}

// newZstdDecompressor fails: this build has no zstd support (built with
// -tags nozstd, e.g. DTAIL_NO_ZSTD=yes), so .zst files can not be read.
func newZstdDecompressor(path string, _ io.Reader) (io.ReadCloser, error) {
	return nil, fmt.Errorf("%s: zstd is not supported in this build (built with -tags nozstd)", path)
}

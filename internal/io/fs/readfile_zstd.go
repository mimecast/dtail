//go:build !nozstd

package fs

import (
	"bufio"
	"io"
	"os"

	"github.com/DataDog/zstd"
)

func (f *ReadFile) makeZstdReader(fd *os.File) (reader *bufio.Reader, decompressor io.Closer, err error) {
	f.logger.Info(f.FilePath(), "Detected zstd compression format")
	zstdReader, err := newZstdDecompressor(f.FilePath(), fd)
	if err != nil {
		return nil, nil, err
	}
	return bufio.NewReader(zstdReader), zstdReader, nil
}

// newZstdDecompressor returns a reader decompressing the zstd stream source
// of the file at path. This build supports zstd, so it never fails.
func newZstdDecompressor(_ string, source io.Reader) (io.ReadCloser, error) {
	return zstd.NewReader(source), nil
}

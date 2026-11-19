//go:build nozstd

package fs

import (
	"bufio"
	"fmt"
	"io"
	"os"
)

func (f *ReadFile) makeZstdReader(fd *os.File) (reader *bufio.Reader, decompressor io.Closer, err error) {
	_ = fd
	err = fmt.Errorf("%s: zstd is not supported in this build (built with -tags nozstd)", f.FilePath())
	return
}

package readhub

import (
	"fmt"
	"os"

	"github.com/mimecast/dtail/internal/io/fs"
)

// position is where a follow read of a file got to: the byte offset just past
// the last line it handled, in the file that file identifies. A negative
// offset means the position is unknown. Offset 0 means the beginning of
// whatever file the path names, e.g. after a rotation or a truncation, so
// file does not matter then.
type position struct {
	offset int64
	file   os.FileInfo
}

func unknownPosition() position {
	return position{offset: -1}
}

// endOfFile returns the position a private follow read of target opened now
// would start at: the current end of the file.
func endOfFile(target fs.ValidatedReadTarget) (position, error) {
	fd, err := target.Open()
	if err != nil {
		return unknownPosition(), fmt.Errorf("open %s for its size: %w", target.ResolvedPath(), err)
	}
	defer func() { _ = fd.Close() }()
	info, err := fd.Stat()
	if err != nil {
		return unknownPosition(), fmt.Errorf("stat %s for its size: %w", target.ResolvedPath(), err)
	}
	return position{offset: info.Size(), file: info}, nil
}

// known reports whether a reader can be positioned at p.
func (p position) known() bool {
	return p.offset == 0 || (p.offset > 0 && p.file != nil)
}

// startAt makes options start the first read of a reader at p: at its offset
// in its file (or at the beginning of the file, should it have been replaced),
// or, when p is unknown, at the end of the file like a new follow read.
func (p position) startAt(options *fs.ReadOptions) {
	switch {
	case !p.known():
		options.SeekEOF = true
	case p.offset > 0:
		options.StartOffset = p.offset
		options.StartOffsetFile = p.file
	}
}

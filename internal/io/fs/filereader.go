package fs

import (
	"context"
	"errors"

	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

// ErrReaderWorkerPanic marks a recovered panic in a FileReader-owned child
// goroutine. Callers must abort that read/session rather than retrying it as a
// transient filesystem or journal error.
var ErrReaderWorkerPanic = errors.New("file reader background worker panic")

// FileReader is the interface used on the dtail server to read/cat/grep/mapr...
// a file. Line delivery is processor-based (line.Processor); the historic
// channel-based reader entry point was removed once every read path migrated
// to the processor pipeline (task iv0).
type FileReader interface {
	Start(ctx context.Context, ltx lcontext.LContext, processor line.Processor, re regex.Regex) error
	FilePath() string
	Retry() bool
}

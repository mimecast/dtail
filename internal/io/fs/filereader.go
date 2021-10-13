package fs

import (
	"context"

	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

// FileReader is the interface used on the dtail server to read/cat/grep/mapr...
// a file.
type FileReader interface {
	Start(ctx context.Context, ltx lcontext.LContext, lines chan<- line.Line,
		re regex.Regex) error
	FilePath() string
	Retry() bool
}

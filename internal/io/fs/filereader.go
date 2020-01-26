package fs

import (
	"context"

	"github.com/mimecast/dtail/internal/io/line"
)

// FileReader is the interface used on the dtail server to read/cat/grep/mapr... a file.
type FileReader interface {
	Start(ctx context.Context, lines chan<- line.Line, regex string) error
	FilePath() string
	Retry() bool
}

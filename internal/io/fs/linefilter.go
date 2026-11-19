package fs

import (
	"context"

	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

// LineFilter applies one session's regex, local context (before, after and
// max) and line numbering to lines fed to it from outside a ReadFile, for
// example by a shared reader that fans one file out to several sessions. It
// uses the same filter as a private ReadFile, so a session sees the same lines
// with the same numbers either way.
//
// A LineFilter has line statistics of its own: the first line fed to it is
// line 1, exactly like the first line a private follow reader reads after
// seeking to the end of the file. Which lines to feed, and in which form, is
// the caller's choice, and must match the private reader of the same mode for
// the output to match: the snapshot reader feeds every line including its
// trailing newline (empty lines too), the follow reader feeds lines without
// the newline and skips empty ones.
//
// A LineFilter is not safe for concurrent use; one goroutine feeds it.
type LineFilter struct {
	stats  stats
	filter *filteringProcessor
}

// NewLineFilter returns a filter that passes matching lines, and the context
// lines ltx asks for, to processor. globID is the source ID passed to the
// processor with every line. Like a private reader it uses processor's
// line.RawProcessor fast path when there is one and ltx has no before-context.
func NewLineFilter(ltx lcontext.LContext, processor line.Processor,
	re regex.Regex, globID string) *LineFilter {

	lineFilter := &LineFilter{}
	lineFilter.filter = newFilteringProcessor(ltx, processor, re, &lineFilter.stats, globID)
	return lineFilter
}

// ProcessLine counts raw as the next line and filters it. raw is borrowed for
// the duration of the call only and is never retained, so the caller may reuse
// its backing array afterwards. stop reports that a max-count limit ended the
// read: the caller must not feed further lines, as a private reader would stop
// reading. err is a processor error, which also ends the read.
func (lf *LineFilter) ProcessLine(raw []byte) (stop bool, err error) {
	return stopForProcessingError(lf.filter.processLine(raw))
}

// Flush flushes the processor. A private follow reader does so after every
// read chunk; a caller feeding a LineFilter should do so after every batch.
func (lf *LineFilter) Flush() error {
	return lf.filter.processor.Flush()
}

// Reopen switches the filter to processor for a new read of the source, as a
// private reader does when it opens the file again after it was rotated or
// its read ended: max-count and local context start afresh, line numbering
// carries on, and before-context lines still buffered are released. The
// caller owns both processors; Reopen neither flushes nor closes the old one.
func (lf *LineFilter) Reopen(processor line.Processor) {
	old := lf.filter
	old.resetGeneration()
	lf.filter = newFilteringProcessor(old.ltx, processor, old.re, &lf.stats, old.globID)
	lf.filter.recycle = old.recycle
}

// Restart handles an in-place truncation of the source, as a private follow
// reader does when it rewinds: local context from the old content is
// discarded, a line.SourceRestarter processor is told that its input starts
// over, and the max-count state and line numbering carry on.
func (lf *LineFilter) Restart() {
	lf.filter.resetGeneration()
	lf.filter.restartSource()
}

// StartFiltered reads the file like Start, but feeds the lines it reads
// through lf, and so to lf's current processor, instead of through a filter of
// its own: line numbering, max-count and local context carry on from the lines
// fed to lf before, for example when a session that was fed by a shared reader
// goes on with a private one. Like Start, it flushes the processor when the
// read ends and releases before-context lines still buffered, except when the
// read ends with ErrHandedOver: lf then keeps its local context for whoever
// goes on feeding it. The reader's own line statistics are not used. lf must
// not be fed from elsewhere meanwhile.
func (f *ReadFile) StartFiltered(ctx context.Context, lf *LineFilter) error {
	return f.start(ctx, lf.filter)
}

// Close releases before-context lines still buffered by the filter. It does
// not close the processor, which the caller owns, just as a private reader
// releases its filter but leaves the processor to the read command.
func (lf *LineFilter) Close() {
	lf.filter.resetGeneration()
}

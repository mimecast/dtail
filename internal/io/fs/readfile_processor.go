package fs

import (
	"bytes"
	"io"

	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

// filteringProcessor wraps a LineProcessor to add regex filtering
type filteringProcessor struct {
	processor line.Processor
	// rawProcessor is processor's optional line.RawProcessor fast path,
	// resolved once at construction (see newFilteringProcessor). Nil means
	// ProcessFilteredRaw falls back to copying matches into a pooled buffer.
	rawProcessor line.RawProcessor
	re           regex.Regex
	ltx          lcontext.LContext
	stats        *stats
	globID       string
	recycle      func(*bytes.Buffer)
	// The query's context is immutable. Select its input path once instead
	// of testing before/after/max settings on every line. Method expressions
	// need no per-filter closure allocation.
	filterRaw func(*filteringProcessor, []byte) error

	// For local context handling
	beforeBuf  []*bytes.Buffer
	beforeHead int // oldest entry when the before-context ring is full
	afterCount int
	maxCount   int
	maxReached bool
}

// newFilteringProcessor builds the filter for one reader or subscriber. stats
// is the line counter the filter numbers lines from; processLine advances it.
func newFilteringProcessor(ltx lcontext.LContext, processor line.Processor,
	re regex.Regex, stats *stats, globID string) *filteringProcessor {

	filterProcessor := &filteringProcessor{
		processor: processor,
		re:        re,
		ltx:       ltx,
		stats:     stats,
		globID:    globID,
	}
	// Max/after context never retains input. Before-context still uses owned
	// buffers, including when emitting the queued lines on a later match.
	if rawProcessor, ok := processor.(line.RawProcessor); ok && ltx.BeforeContext <= 0 {
		filterProcessor.rawProcessor = rawProcessor
	}
	switch {
	case ltx.BeforeContext > 0:
		filterProcessor.filterRaw = (*filteringProcessor).processOwnedContext
	case ltx.Has():
		filterProcessor.filterRaw = (*filteringProcessor).processRawContext
	default:
		filterProcessor.filterRaw = (*filteringProcessor).ProcessFilteredRaw
	}
	return filterProcessor
}

func (f *ReadFile) recycleBytesBuffer(buf *bytes.Buffer) {
	pool.RecycleBytesBuffer(buf)
	if f.bufferRecycleObserver != nil {
		f.bufferRecycleObserver(buf)
	}
}

func (fp *filteringProcessor) recycleBytesBuffer(buf *bytes.Buffer) {
	if fp.recycle != nil {
		fp.recycle(buf)
		return
	}
	pool.RecycleBytesBuffer(buf)
}

// resetGeneration discards local context that belongs to the file generation
// that just ended. Match-count state remains query-wide across an in-place
// truncation boundary.
func (fp *filteringProcessor) resetGeneration() {
	for i, lineBuf := range fp.beforeBuf {
		if lineBuf != nil {
			fp.recycleBytesBuffer(lineBuf)
			fp.beforeBuf[i] = nil
		}
	}
	fp.beforeBuf = fp.beforeBuf[:0]
	fp.beforeHead = 0
	fp.afterCount = 0
}

// restartSource tells the wrapped processor, when it is a
// line.SourceRestarter, that the content it is fed from starts over (an
// in-place truncation). Local context is reset separately by resetGeneration.
func (fp *filteringProcessor) restartSource() {
	if restarter, ok := fp.processor.(line.SourceRestarter); ok {
		restarter.SourceRestarted()
	}
}

// processLine counts data as the next line and filters it. data is borrowed
// for the duration of the call only: without local context it takes the
// zero-copy ProcessFilteredRaw path. Max/after context also borrows data;
// before-context uses an owned pooled buffer. Every reader and subscriber
// feeds its lines through here, so line numbering and filtering cannot diverge.
func (fp *filteringProcessor) processLine(data []byte) error {
	fp.stats.updatePosition()
	return fp.filterRaw(fp, data)
}

func (fp *filteringProcessor) processOwnedContext(data []byte) error {
	lineBuf := pool.BytesBuffer.Get().(*bytes.Buffer)
	lineBuf.Write(data)
	return fp.processWithContext(lineBuf, fp.stats.totalLineCount())
}

// ProcessFilteredRaw is the zero-copy path when there is no local context.
// It runs the regex match directly on the scanner-owned byte slice, so a
// non-matching line costs no pooled buffer at all. At low hit rates this avoids
// a pool.Get + copy + pool.Put for the (vast majority of) non-matching lines,
// which profiling showed as ~10-15% of serverless dgrep CPU (sync.Pool Get/Put +
// bytes.Buffer.Write).
//
// It is selected only without local context. Max/after-only queries use
// processRawContext; before-context must use owned input because it buffers
// non-matching lines beyond the call.
//
// The caller passes raw = scanner.Bytes() (or the follow reader's partial-line
// buffer), which is only valid until the next Scan() or Reset. A match goes to
// the processor's line.RawProcessor fast path when it has one: that contract
// borrows raw for the duration of the call only, so no copy and no pooled
// buffer is needed. Otherwise the match is copied into a pooled buffer, which
// is a stable copy that never aliases the scanner's transient slice.
func (fp *filteringProcessor) ProcessFilteredRaw(raw []byte) error {
	if !fp.re.Match(raw) {
		fp.stats.updateFilteredLine(false)
		// No buffer was acquired, so there is nothing to recycle.
		return nil
	}

	fp.stats.updateFilteredLine(true)

	// Keep this hot path inline: routing every no-context match through
	// emitRaw adds a measurable call cost for short, dense reads.
	lineNum := fp.stats.totalLineCount()
	if fp.rawProcessor != nil {
		return fp.rawProcessor.ProcessRawLine(raw, lineNum, fp.globID)
	}
	lineBuf := pool.BytesBuffer.Get().(*bytes.Buffer)
	lineBuf.Write(raw)
	// Ownership transfers even on errors/panics; only the processor recycles.
	return fp.processor.ProcessLine(lineBuf, lineNum, fp.globID)
}

// processRawContext filters max/after-only reads without copying rejected
// input. Neither mode retains lines, so emitted input can also be borrowed.
func (fp *filteringProcessor) processRawContext(raw []byte) error {
	if !fp.re.Match(raw) {
		fp.stats.updateLineNotMatched()
		if fp.ltx.AfterContext > 0 && fp.afterCount > 0 {
			fp.afterCount--
			fp.stats.updateLineTransmitted()
			return fp.emitRaw(raw)
		}
		fp.stats.updateLineNotTransmitted()
		return nil
	}
	fp.stats.updateLineMatched()
	if fp.maxReached {
		return io.EOF
	}
	fp.stats.updateLineTransmitted()
	if err := fp.emitRaw(raw); err != nil {
		return err
	}
	return fp.finishContextMatch()
}

func (fp *filteringProcessor) emitRaw(raw []byte) error {
	lineNum := fp.stats.totalLineCount()
	if fp.rawProcessor != nil {
		return fp.rawProcessor.ProcessRawLine(raw, lineNum, fp.globID)
	}
	// A processor without the borrowed interface owns this copy, including
	// on errors/panics. The filter must never recycle it after transfer.
	buf := pool.BytesBuffer.Get().(*bytes.Buffer)
	buf.Write(raw)
	return fp.processor.ProcessLine(buf, lineNum, fp.globID)
}

// retainBefore keeps the last BeforeContext owned lines without advancing
// the slice's backing array. Storage grows only until the ring is full.
func (fp *filteringProcessor) retainBefore(buf *bytes.Buffer) {
	if len(fp.beforeBuf) < fp.ltx.BeforeContext {
		fp.beforeBuf = append(fp.beforeBuf, buf)
		return
	}
	fp.recycleBytesBuffer(fp.beforeBuf[fp.beforeHead])
	fp.beforeBuf[fp.beforeHead] = buf
	fp.beforeHead++
	if fp.beforeHead == len(fp.beforeBuf) {
		fp.beforeHead = 0
	}
}

func (fp *filteringProcessor) emitBefore(lineNum uint64) error {
	count := len(fp.beforeBuf)
	for i := 0; i < count; i++ {
		index := (fp.beforeHead + i) % count
		buf := fp.beforeBuf[index]
		// Clear before transfer: error/panic cleanup must only recycle the
		// remaining locally owned buffers, never the processor's input.
		fp.beforeBuf[index] = nil
		fp.stats.updateLineTransmitted()
		if err := fp.processor.ProcessLine(buf, lineNum-uint64(count-i), fp.globID); err != nil {
			fp.resetGeneration()
			return err
		}
	}
	fp.beforeBuf = fp.beforeBuf[:0]
	fp.beforeHead = 0
	return nil
}

// processWithContext handles lines when local context is enabled
func (fp *filteringProcessor) processWithContext(rawLine *bytes.Buffer, lineNum uint64) error {
	// rawLine remains ours until it is buffered, recycled, or handed to the
	// underlying processor. Keep that ownership explicit so a panic while
	// emitting before-context cannot strand the still-pending matching line.
	ownedRawLine := rawLine
	defer func() {
		if ownedRawLine != nil {
			fp.recycleBytesBuffer(ownedRawLine)
		}
	}()

	matched := fp.re.Match(rawLine.Bytes())

	if !matched {
		fp.stats.updateLineNotMatched()

		// Handle after context
		if fp.ltx.AfterContext > 0 && fp.afterCount > 0 {
			fp.afterCount--
			fp.stats.updateLineTransmitted()
			// Ownership transfers to the processor, which recycles rawLine on every
			// return path; recycling here on error would double Put into the shared
			// pool and allow two concurrent readers to receive the same buffer.
			ownedRawLine = nil
			return fp.processor.ProcessLine(rawLine, lineNum, fp.globID)
		}

		// Handle before context buffer
		if fp.ltx.BeforeContext > 0 {
			fp.retainBefore(rawLine)
			ownedRawLine = nil
		} else {
			ownedRawLine = nil
			fp.recycleBytesBuffer(rawLine)
		}

		fp.stats.updateLineNotTransmitted()
		return nil
	}

	// Line matched
	fp.stats.updateLineMatched()

	// Check if we've reached max count
	if fp.maxReached {
		ownedRawLine = nil
		fp.recycleBytesBuffer(rawLine)
		return io.EOF // Stop processing
	}

	// Process before context
	if fp.ltx.BeforeContext > 0 && len(fp.beforeBuf) > 0 {
		if err := fp.emitBefore(lineNum); err != nil {
			return err
		}
	}

	// Process the matched line. Ownership transfers to the processor, which
	// recycles rawLine on every return path; recycling here on error would double
	// Put into the shared pool and allow two readers to receive the same buffer.
	fp.stats.updateLineTransmitted()
	ownedRawLine = nil
	if err := fp.processor.ProcessLine(rawLine, lineNum, fp.globID); err != nil {
		return err
	}
	return fp.finishContextMatch()
}

// finishContextMatch advances query state only after a successful emission.
// With max+after, the legacy stop boundary is the NEXT match, even if the
// requested trailing context ended earlier. Both input paths preserve it.
func (fp *filteringProcessor) finishContextMatch() error {
	// Update max count
	if fp.ltx.MaxCount > 0 {
		fp.maxCount++
		if fp.maxCount >= fp.ltx.MaxCount {
			if fp.ltx.AfterContext == 0 {
				return io.EOF // Stop processing
			}
			fp.maxReached = true
		}
	}

	// Reset after context
	if fp.ltx.AfterContext > 0 {
		fp.afterCount = fp.ltx.AfterContext
	}

	return nil
}

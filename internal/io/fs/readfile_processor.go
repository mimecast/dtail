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
	re        regex.Regex
	ltx       lcontext.LContext
	stats     *stats
	globID    string
	recycle   func(*bytes.Buffer)

	// For local context handling
	beforeBuf  []*bytes.Buffer
	afterCount int
	maxCount   int
	maxReached bool
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
	fp.afterCount = 0
}

// ProcessFilteredLine applies regex filtering before passing to the underlying processor
func (fp *filteringProcessor) ProcessFilteredLine(rawLine *bytes.Buffer) error {
	ownedRawLine := rawLine
	defer func() {
		if ownedRawLine != nil {
			fp.recycleBytesBuffer(ownedRawLine)
		}
	}()

	// Update stats
	lineNum := fp.stats.totalLineCount()

	// Simple case: no local context
	if !fp.ltx.Has() {
		if !fp.re.Match(rawLine.Bytes()) {
			fp.stats.updateLineNotMatched()
			fp.stats.updateLineNotTransmitted()
			ownedRawLine = nil
			fp.recycleBytesBuffer(rawLine)
			return nil
		}

		fp.stats.updateLineMatched()
		fp.stats.updateLineTransmitted()

		// Process the line. Per the line.Processor contract (processor.go),
		// ownership of rawLine transfers to the processor, which recycles it on
		// every return path. The only processors on the fs read path -
		// DirectLineProcessor and AggregateProcessor - recycle unconditionally,
		// even when ProcessLine returns a write error (e.g. a client disconnect /
		// broken pipe). Recycling here on error would Put the same buffer into the
		// shared pool.BytesBuffer a second time; the pool would then hand one object
		// to two Get callers whose concurrent writes race and corrupt data. So do
		// not recycle rawLine here.
		ownedRawLine = nil
		return fp.processor.ProcessLine(rawLine, lineNum, fp.globID)
	}

	// Complex case: handle local context (before/after/max)
	ownedRawLine = nil
	return fp.processWithContext(rawLine, lineNum)
}

// ProcessFilteredRaw is the zero-copy fast path for the no-local-context case.
// It runs the regex match directly on the scanner-owned byte slice and only
// acquires+fills a pooled buffer when the line actually matches. At low hit
// rates this avoids a pool.Get + copy + pool.Put for the (vast majority of)
// non-matching lines, which profiling showed as ~10-15% of serverless
// dgrep CPU (sync.Pool Get/Put + bytes.Buffer.Write).
//
// Semantics are identical to the !ltx.Has() branch of ProcessFilteredLine: the
// same regex, the same stats bookkeeping, and the same lineNum are used, so
// output is byte-identical. It MUST only be called when fp.ltx.Has() is false;
// the local-context path deliberately buffers non-matching lines (before/after
// context) and cannot skip the copy.
//
// The caller passes raw = scanner.Bytes(), which is only valid until the next
// Scan(). On a match we copy it into a pooled buffer before returning, so the
// buffer handed to the underlying processor is a stable copy and never aliases
// the scanner's transient slice.
func (fp *filteringProcessor) ProcessFilteredRaw(raw []byte) error {
	lineNum := fp.stats.totalLineCount()

	if !fp.re.Match(raw) {
		fp.stats.updateLineNotMatched()
		fp.stats.updateLineNotTransmitted()
		// No buffer was acquired, so there is nothing to recycle.
		return nil
	}

	fp.stats.updateLineMatched()
	fp.stats.updateLineTransmitted()

	// Only now, on a confirmed match, pay for the buffer and the copy.
	lineBuf := pool.BytesBuffer.Get().(*bytes.Buffer)
	lineBuf.Write(raw)

	// Ownership of lineBuf transfers to the processor, which recycles it on every
	// return path (see ProcessFilteredLine for the full rationale). Recycling here
	// on error would return the same buffer to the shared pool a second time and
	// race, so leave it to the processor.
	return fp.processor.ProcessLine(lineBuf, lineNum, fp.globID)
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
			// pool and race (see ProcessFilteredLine).
			ownedRawLine = nil
			return fp.processor.ProcessLine(rawLine, lineNum, fp.globID)
		}

		// Handle before context buffer
		if fp.ltx.BeforeContext > 0 {
			// Add to before buffer
			if len(fp.beforeBuf) >= fp.ltx.BeforeContext {
				// Recycle oldest buffer
				fp.recycleBytesBuffer(fp.beforeBuf[0])
				fp.beforeBuf = fp.beforeBuf[1:]
			}
			ownedRawLine = nil
			fp.beforeBuf = append(fp.beforeBuf, rawLine)
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
		beforeCount := len(fp.beforeBuf)
		for i, buf := range fp.beforeBuf {
			// Ownership transfers to the processor before the call. Clearing the
			// slot lets resetGeneration recycle only buffers still owned here if
			// the processor returns an error or panics.
			fp.beforeBuf[i] = nil
			fp.stats.updateLineTransmitted()
			if err := fp.processor.ProcessLine(buf, lineNum-uint64(beforeCount-i), fp.globID); err != nil {
				fp.resetGeneration()
				return err
			}
		}
		fp.beforeBuf = fp.beforeBuf[:0]
	}

	// Process the matched line. Ownership transfers to the processor, which
	// recycles rawLine on every return path; recycling here on error would double
	// Put into the shared pool and race (see ProcessFilteredLine).
	fp.stats.updateLineTransmitted()
	ownedRawLine = nil
	if err := fp.processor.ProcessLine(rawLine, lineNum, fp.globID); err != nil {
		return err
	}

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

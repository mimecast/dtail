package fs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mimecast/dtail/internal/ctxutil"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

const maxReadScannerTokenSize = 1024 * 1024

type followLineProcessor struct {
	file        *ReadFile
	filter      *filteringProcessor
	partialLine *bytes.Buffer
	hasContext  bool
}

// readWithProcessorOptimized reads from the file using buffered line reading
// instead of byte-by-byte reading for better performance
func (f *ReadFile) readWithProcessorOptimized(ctx context.Context, fd *os.File, reader *bufio.Reader,
	truncate <-chan struct{}, ltx lcontext.LContext, processor line.Processor, re regex.Regex) error {

	filterProcessor := f.newFilteringProcessor(ltx, processor, re)
	defer filterProcessor.resetGeneration()

	hasContext := ltx.Has()
	scanner := bufio.NewScanner(reader)
	bufPtr := pool.GetScannerBuffer()
	defer pool.PutScannerBuffer(bufPtr)
	scanner.Buffer(*bufPtr, maxReadScannerTokenSize)

	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		return f.scanLinesWithMaxLength(ctx, data, atEOF)
	})

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if err := f.checkSnapshotTruncation(fd, truncate); err != nil {
			return err
		}
		if err := f.processSnapshotLine(filterProcessor, hasContext, scanner.Bytes()); err != nil {
			if isEarlyStop(err) {
				return nil
			}
			return err
		}
	}

	if err := scanner.Err(); err != nil {
		if errors.Is(err, io.EOF) && f.follow {
			return nil
		}
		return err
	}

	return nil
}

func (f *ReadFile) newFilteringProcessor(ltx lcontext.LContext,
	processor line.Processor, re regex.Regex) *filteringProcessor {

	filterProcessor := &filteringProcessor{
		processor: processor,
		re:        re,
		ltx:       ltx,
		stats:     &f.stats,
		globID:    f.globID,
	}
	// The raw fast path is only valid without local context; ProcessFilteredRaw
	// is not called in that case, but leave the field nil so the precondition
	// does not rest on the callers alone.
	if rawProcessor, ok := processor.(line.RawProcessor); ok && !ltx.Has() {
		filterProcessor.rawProcessor = rawProcessor
	}
	if f.bufferRecycleObserver != nil {
		filterProcessor.recycle = f.recycleBytesBuffer
	}
	return filterProcessor
}

// checkSnapshotTruncation handles the periodic signal outside the per-line
// processing concern. The empty-channel path remains one non-blocking receive.
func (f *ReadFile) checkSnapshotTruncation(fd *os.File, truncate <-chan struct{}) error {
	select {
	case <-truncate:
		if truncated, err := f.truncated(fd); truncated {
			return err
		}
	default:
	}
	return nil
}

func (f *ReadFile) processSnapshotLine(filterProcessor *filteringProcessor,
	hasContext bool, data []byte) error {

	f.updatePosition()
	if !hasContext {
		return filterProcessor.ProcessFilteredRaw(data)
	}

	lineBuf := pool.BytesBuffer.Get().(*bytes.Buffer)
	lineBuf.Write(data)
	return filterProcessor.ProcessFilteredLine(lineBuf)
}

// isEarlyStop reports whether err is the io.EOF sentinel that filteringProcessor
// returns from processWithContext once a max-count (-m/-max) limit is reached.
// This is a NORMAL early stop, not a genuine I/O error: bufio.Scanner signals
// real end-of-input via Scan()==false and never returns io.EOF from
// ProcessFiltered*, so any io.EOF bubbling up from the filter can only be the
// max-count sentinel. It must be swallowed here, otherwise it leaks out to the
// caller and is logged as a spurious SERVER|...|ERROR|...|EOF line. Real
// (non-EOF) processor errors are left untouched so they still surface.
//
// Bare equality (== io.EOF) is intentional and must NOT become errors.Is: the
// sentinel is returned bare by processWithContext, so exact identity matches it
// precisely. A WRAPPED io.EOF, by contrast, can only originate from a genuine
// downstream failure (e.g. an ssh channel Write after the peer closed), which we
// deliberately do NOT want to mistake for a clean early stop.
func isEarlyStop(err error) bool {
	return err == io.EOF //nolint:errorlint // Wrapped EOF is a downstream write failure, not the private early-stop sentinel.
}

// scanLinesWithMaxLength is a custom split function for bufio.Scanner that respects MaxLineLength.
// It is kept context-aware so long-line warnings can still be dropped when the reader is canceled.
func (f *ReadFile) scanLinesWithMaxLength(ctx context.Context, data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	maxLineLen := f.lineLimit()

	// Look for a newline
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		// Check if the line before the newline exceeds max length
		if i > maxLineLen {
			// Line is too long, split it at maxLineLen
			if !f.warnAboutLongLine(ctx) {
				return 0, nil, ctx.Err()
			}
			return maxLineLen, data[0:maxLineLen], nil
		}
		// We have a full line within the limit
		f.warnedAboutLongLine = false // Reset warning for next long line sequence
		return i + 1, data[0 : i+1], nil
	}

	// If we're at EOF, we have a final, non-terminated line
	if atEOF {
		if len(data) > maxLineLen {
			// Even at EOF, respect max line length
			if !f.warnAboutLongLine(ctx) {
				return 0, nil, ctx.Err()
			}
			return maxLineLen, data[0:maxLineLen], nil
		}
		return len(data), data, nil
	}

	// If the line is too long, split it
	if len(data) >= maxLineLen {
		// Warn about long line (only once)
		if !f.warnAboutLongLine(ctx) {
			return 0, nil, ctx.Err()
		}

		// Return a chunk up to MaxLineLength
		return maxLineLen, data[0:maxLineLen], nil
	}

	// Request more data
	return 0, nil, nil
}

// Start reads a log file using buffered line reading and a line processor.
func (f *ReadFile) Start(ctx context.Context, ltx lcontext.LContext,
	processor line.Processor, re regex.Regex) error {

	truncateCtx, cancelTruncate := context.WithCancel(ctx)
	defer cancelTruncate()

	reader, fd, decompressor, err := f.makeReader(truncateCtx)
	if fd != nil {
		defer func() { _ = fd.Close() }()
	}
	if decompressor != nil {
		defer func() {
			if closeErr := decompressor.Close(); closeErr != nil {
				f.logger.Warn(f.filePath, "Unable to close compressed reader", closeErr)
			}
		}()
	}
	if err != nil {
		return err
	}

	truncate := make(chan struct{})
	truncateDone := f.startPeriodicTruncateCheck(truncateCtx, cancelTruncate, truncate)

	// For tail mode, we need to handle continuous reading
	if f.follow {
		err = f.tailWithProcessorOptimized(truncateCtx, fd, reader, truncate, ltx, processor, re)
	} else {
		// For cat/grep mode, just read once
		err = f.readWithProcessorOptimized(truncateCtx, fd, reader, truncate, ltx, processor, re)
	}

	cancelTruncate()
	truncateErr := <-truncateDone
	if truncateErr != nil {
		err = errors.Join(err, truncateErr)
	} else if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		err = nil
	}

	// Ensure any buffered data is flushed
	if flushErr := processor.Flush(); flushErr != nil && err == nil {
		err = flushErr
	}

	return err
}

// tailWithProcessorOptimized handles continuous reading for tail mode
func (f *ReadFile) tailWithProcessorOptimized(ctx context.Context, fd *os.File, reader *bufio.Reader,
	truncate <-chan struct{}, ltx lcontext.LContext, processor line.Processor, re regex.Regex) error {

	filterProcessor := f.newFilteringProcessor(ltx, processor, re)
	defer filterProcessor.resetGeneration()

	partialLine := pool.BytesBuffer.Get().(*bytes.Buffer)
	defer pool.RecycleBytesBuffer(partialLine)
	lineProcessor := followLineProcessor{
		file:        f,
		filter:      filterProcessor,
		partialLine: partialLine,
		hasContext:  ltx.Has(),
	}

	bufPtr := pool.GetMediumBuffer()
	defer pool.PutMediumBuffer(bufPtr)

	for {
		buf := (*bufPtr)[:cap(*bufPtr)]
		n, readErr := reader.Read(buf)

		if n > 0 {
			stop, err := lineProcessor.processChunk(ctx, buf[:n])
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
			if flushErr := processor.Flush(); flushErr != nil {
				return flushErr
			}
		}

		if readErr != nil {
			keepReading, err := lineProcessor.handleReadError(ctx, fd, reader, truncate, readErr)
			if err != nil {
				return err
			}
			if !keepReading {
				return nil
			}
		}

		if ctx.Err() != nil {
			return lineProcessor.finish()
		}
	}
}

func (p *followLineProcessor) processChunk(ctx context.Context, data []byte) (bool, error) {
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			return p.processFragment(ctx, data)
		}

		p.partialLine.Write(data[:newline])
		if p.partialLine.Len() > 0 {
			if stop, err := stopForProcessingError(p.processPartialLine()); stop || err != nil {
				return stop, err
			}
		}
		p.partialLine.Reset()
		p.file.warnedAboutLongLine = false
		data = data[newline+1:]
	}
	return false, nil
}

func (p *followLineProcessor) processFragment(ctx context.Context, data []byte) (bool, error) {
	p.partialLine.Write(data)
	if p.partialLine.Len() < p.file.lineLimit() {
		return false, nil
	}
	if !p.file.warnAboutLongLine(ctx) {
		return true, nil
	}

	stop, err := stopForProcessingError(p.processPartialLine())
	if !stop && err == nil {
		p.partialLine.Reset()
	}
	return stop, err
}

func (p *followLineProcessor) processPartialLine() error {
	p.file.updatePosition()
	if !p.hasContext {
		return p.filter.ProcessFilteredRaw(p.partialLine.Bytes())
	}

	lineBuf := pool.BytesBuffer.Get().(*bytes.Buffer)
	lineBuf.Write(p.partialLine.Bytes())
	return p.filter.ProcessFilteredLine(lineBuf)
}

func (p *followLineProcessor) handleReadError(ctx context.Context, fd *os.File,
	reader *bufio.Reader, truncate <-chan struct{}, readErr error) (bool, error) {

	if !errors.Is(readErr, io.EOF) {
		return false, readErr
	}
	if ctx.Err() != nil {
		return false, nil
	}

	truncated, err := p.file.truncated(fd)
	if truncated {
		return p.handleTruncation(fd, reader, err)
	}

	select {
	case <-truncate:
	default:
	}
	return ctxutil.Sleep(ctx, 100*time.Millisecond), nil
}

func (p *followLineProcessor) handleTruncation(fd *os.File, reader *bufio.Reader,
	truncateErr error) (bool, error) {

	p.file.warnedAboutLongLine = false
	if !errors.Is(truncateErr, errFileTruncated) {
		return false, truncateErr
	}
	if _, err := fd.Seek(0, io.SeekStart); err != nil {
		return false, fmt.Errorf("rewind truncated file %s: %w", p.file.FilePath(), err)
	}

	reader.Reset(fd)
	p.partialLine.Reset()
	p.filter.resetGeneration()
	// The same processor keeps being fed, now from the rewritten content:
	// let a processor with per-source state (the CSV header of a MapReduce
	// aggregation) start over for it.
	p.filter.restartSource()
	p.file.logger.Info(p.file.FilePath(), "File got truncated, reading from beginning")
	return true, nil
}

func (p *followLineProcessor) finish() error {
	if p.partialLine.Len() == 0 {
		return nil
	}
	err := p.processPartialLine()
	if isEarlyStop(err) {
		return nil
	}
	return err
}

func stopForProcessingError(err error) (bool, error) {
	if isEarlyStop(err) {
		return true, nil
	}
	return false, err
}

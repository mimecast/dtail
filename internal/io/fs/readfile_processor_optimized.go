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

// After a follow read found its file rotated away from the path, it reads the
// old file to its end, then polls it rotationDrainPollInterval later for lines
// that a writer which has not reopened the path yet still appends, again after
// every poll that found some, but at most maxRotationDrainPolls times, so that
// such a writer cannot keep the reader from moving on to the new file.
const (
	rotationDrainPollInterval = 100 * time.Millisecond
	maxRotationDrainPolls     = 10
)

type followLineProcessor struct {
	file        *ReadFile
	filter      *filteringProcessor
	processor   line.Processor
	reader      *bufio.Reader
	buf         []byte
	partialLine *bytes.Buffer
	// positions tells a line.PositionObserver where each line ends; nil for
	// every other processor.
	positions *positionReporter
	// atLineStart reports that everything read so far ended with a newline
	// (or the read started at the beginning of a line), so that nothing of
	// the line the reader is in was read yet. Only tracked for a read that
	// may hand over at the end of the file.
	atLineStart bool
}

// readWithProcessorOptimized reads from the file using buffered line reading
// instead of byte-by-byte reading for better performance
func (f *ReadFile) readWithProcessorOptimized(ctx context.Context, fd *os.File, reader *bufio.Reader,
	truncate <-chan struct{}, filterProcessor *filteringProcessor) error {

	defer filterProcessor.resetGeneration()

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
		if err := filterProcessor.processLine(scanner.Bytes()); err != nil {
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

	filterProcessor := newFilteringProcessor(ltx, processor, re, &f.stats, f.globID)
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

	return f.start(ctx, f.newFilteringProcessor(ltx, processor, re))
}

// start reads the file through filterProcessor, which numbers, filters and
// passes the lines on to its processor.
func (f *ReadFile) start(ctx context.Context, filterProcessor *filteringProcessor) error {
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
		err = f.tailWithProcessorOptimized(truncateCtx, fd, reader, truncate, filterProcessor)
	} else {
		// For cat/grep mode, just read once
		err = f.readWithProcessorOptimized(truncateCtx, fd, reader, truncate, filterProcessor)
	}

	cancelTruncate()
	truncateErr := <-truncateDone
	if truncateErr != nil {
		err = errors.Join(err, truncateErr)
	} else if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		err = nil
	}

	// Ensure any buffered data is flushed
	if flushErr := filterProcessor.processor.Flush(); flushErr != nil && err == nil {
		err = flushErr
	}

	return err
}

// tailWithProcessorOptimized handles continuous reading for tail mode
func (f *ReadFile) tailWithProcessorOptimized(ctx context.Context, fd *os.File, reader *bufio.Reader,
	truncate <-chan struct{}, filterProcessor *filteringProcessor) (err error) {

	defer func() {
		// A read handed over keeps its local context for whoever goes on
		// feeding the filter.
		if !errors.Is(err, ErrHandedOver) {
			filterProcessor.resetGeneration()
		}
	}()
	processor := filterProcessor.processor

	positions, positionsErr := f.newPositionReporter(fd, reader, processor)
	if positionsErr != nil {
		return positionsErr
	}

	partialLine := pool.BytesBuffer.Get().(*bytes.Buffer)
	defer pool.RecycleBytesBuffer(partialLine)
	bufPtr := pool.GetMediumBuffer()
	defer pool.PutMediumBuffer(bufPtr)
	lineProcessor := followLineProcessor{
		file:        f,
		filter:      filterProcessor,
		processor:   processor,
		reader:      reader,
		buf:         (*bufPtr)[:cap(*bufPtr)],
		partialLine: partialLine,
		positions:   positions,
		atLineStart: f.handOverAtEOF != nil && startsLine(fd),
	}

	for {
		_, readErr, stop, err := lineProcessor.read(ctx)
		if err != nil {
			return err
		}
		if stop {
			return nil
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

// read reads the next bytes of the file, as many as the buffer holds, feeds
// the lines they complete and flushes the processor. It returns how many bytes
// it read and the error the read returned, and reports whether the read
// stops because a max-count limit was reached, or the error that ends it.
func (p *followLineProcessor) read(ctx context.Context) (n int, readErr error, stop bool, err error) {
	p.positions.readStarting()
	n, readErr = p.reader.Read(p.buf)
	if err := p.positions.readReturned(n); err != nil {
		return n, readErr, false, err
	}
	if n == 0 {
		return n, readErr, false, nil
	}
	if stop, err := p.processChunk(ctx, p.buf[:n]); stop || err != nil {
		return n, readErr, stop, err
	}
	return n, readErr, false, p.processor.Flush()
}

// processChunk feeds the lines that data, the bytes of one read, completes.
func (p *followLineProcessor) processChunk(ctx context.Context, data []byte) (bool, error) {
	consumed := 0 // bytes of data before the rest still to process
	for consumed < len(data) {
		rest := data[consumed:]
		newline := bytes.IndexByte(rest, '\n')
		if newline < 0 {
			p.atLineStart = false
			return p.processFragment(ctx, rest, len(data))
		}

		consumed += newline + 1
		p.atLineStart = true
		p.partialLine.Write(rest[:newline])
		if p.partialLine.Len() > 0 {
			if stop, err := stopForProcessingError(p.processPartialLine(consumed)); stop || err != nil {
				return stop, err
			}
		}
		p.partialLine.Reset()
		p.file.warnedAboutLongLine = false
	}
	return false, nil
}

// processFragment keeps fragment, the unfinished end of a read of readLen
// bytes, unless the pending line grew as long as the maximum line length.
func (p *followLineProcessor) processFragment(ctx context.Context, fragment []byte, readLen int) (bool, error) {
	p.partialLine.Write(fragment)
	if p.partialLine.Len() < p.file.lineLimit() {
		return false, nil
	}
	if !p.file.warnAboutLongLine(ctx) {
		return true, nil
	}

	stop, err := stopForProcessingError(p.processPartialLine(readLen))
	if !stop && err == nil {
		p.partialLine.Reset()
	}
	return stop, err
}

// processPartialLine feeds the pending line, which ends after the first
// readEnd bytes of the current read.
func (p *followLineProcessor) processPartialLine(readEnd int) error {
	err := p.filter.processLine(p.partialLine.Bytes())
	p.positions.lineEndsAt(readEnd)
	return err
}

func (p *followLineProcessor) handleReadError(ctx context.Context, fd *os.File,
	reader *bufio.Reader, truncate <-chan struct{}, readErr error) (bool, error) {

	if !errors.Is(readErr, io.EOF) {
		return false, readErr
	}
	if ctx.Err() != nil {
		return false, nil
	}

	truncated, offset, file, err := p.file.inspectOpenFile(fd)
	if truncated && leftPath(err) {
		if keepGoing, drainErr := p.drainRotatedFile(ctx); !keepGoing || drainErr != nil {
			return false, drainErr
		}
	}
	if truncated {
		return p.handleTruncation(fd, reader, err)
	}
	if p.handsOver(reader, offset, file) {
		return false, ErrHandedOver
	}

	select {
	case <-truncate:
	default:
	}
	return ctxutil.Sleep(ctx, 100*time.Millisecond), nil
}

// leftPath reports whether err, from inspectOpenFile, says that the path no
// longer names the open file: it was rotated, or removed and not recreated
// yet, as between the rename and the create of a rotation.
func leftPath(err error) bool {
	return errors.Is(err, errFileRotated) || errors.Is(err, os.ErrNotExist)
}

// drainRotatedFile feeds what was appended to the open file, which the path
// was rotated away from, since the reader found the end of it: what a writer
// appended before the rotation but after the reader's last read, and what a
// writer that has not reopened the path yet appends shortly after the
// rotation (see rotationDrainPollInterval). The lines keep their order,
// numbering and positions in the old file. A last line of the old file that
// is still unfinished when draining ends is dropped, as a follow read always
// dropped the pending line at a rotation: every line a follow read feeds with
// a position ends at a newline or where it split a line longer than the
// maximum line length, which shared readers rely on (see readhub), and a
// writer may still finish it. It returns false, with the error that ends the
// read, if any, when the read ends while draining: canceled, or stopped by a
// max-count limit.
func (p *followLineProcessor) drainRotatedFile(ctx context.Context) (bool, error) {
	for polls := 0; ; polls++ {
		got, keepGoing, err := p.readToEnd(ctx)
		if !keepGoing || err != nil {
			return false, err
		}
		if (polls > 0 && !got) || polls == maxRotationDrainPolls {
			return true, nil
		}
		if !p.file.pollRotatedFile(ctx) {
			return false, nil
		}
	}
}

// readToEnd reads and feeds the open file up to its current end. It reports
// whether it read anything, and returns false, with the error that ends the
// read, if any, when the read ends.
func (p *followLineProcessor) readToEnd(ctx context.Context) (got, keepGoing bool, err error) {
	for ctx.Err() == nil {
		n, readErr, stop, err := p.read(ctx)
		got = got || n > 0
		switch {
		case err != nil:
			return got, false, err
		case stop:
			return got, false, nil
		case errors.Is(readErr, io.EOF):
			return got, true, nil
		case readErr != nil:
			return got, false, readErr
		}
	}
	return got, false, nil
}

// handsOver reports whether the read, which reached the end of the file at
// offset, ends here because ReadOptions.HandOverAtEOF took it over. It is only
// asked at the start of a line, with every line read fed.
func (p *followLineProcessor) handsOver(reader *bufio.Reader, offset int64, file os.FileInfo) bool {
	if p.file.handOverAtEOF == nil || file == nil || !p.atLineStart ||
		p.partialLine.Len() > 0 || reader.Buffered() > 0 {
		return false
	}
	return p.file.handOverAtEOF(offset, file)
}

// startsLine reports whether fd is positioned at the beginning of a line:
// at the beginning of the file, or just past a newline.
func startsLine(fd *os.File) bool {
	if fd == nil {
		return false
	}
	offset, err := fd.Seek(0, io.SeekCurrent)
	if err != nil {
		return false
	}
	if offset == 0 {
		return true
	}
	var last [1]byte
	if _, err := fd.ReadAt(last[:], offset-1); err != nil {
		return false
	}
	return last[0] == '\n'
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
	p.atLineStart = true
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
	// A fragment fed on cancellation has no position (see
	// line.PositionObserver).
	err := p.filter.processLine(p.partialLine.Bytes())
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

// positionReporter tells a line.PositionObserver processor where each line
// it was fed ends. A nil reporter reports nothing, which is the case for every
// other processor, for compressed files and for the stdin pipe.
type positionReporter struct {
	observer line.PositionObserver
	fd       *os.File
	reader   *bufio.Reader
	file     os.FileInfo
	// readStart is the file offset of the first byte of the current read.
	readStart int64
}

// newPositionReporter stats the open file once: its identity does not change
// while one reader runs, because a rotation ends the read.
func (f *ReadFile) newPositionReporter(fd *os.File, reader *bufio.Reader,
	processor line.Processor) (*positionReporter, error) {

	observer, ok := processor.(line.PositionObserver)
	if !ok || fd == nil || CompressionFormat(f.FilePath()) != "" {
		return nil, nil
	}
	file, err := fd.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s for line positions: %w", f.filePath, err)
	}
	start, err := fd.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, fmt.Errorf("read position of %s: %w", f.filePath, err)
	}
	observer.ReadUpTo(start, file)
	return &positionReporter{observer: observer, fd: fd, reader: reader, file: file}, nil
}

// readStarting tells the observer that the reader is about to read.
func (r *positionReporter) readStarting() {
	if r != nil {
		r.observer.ReadStarting()
	}
}

// readReturned tells the observer how far the reader has read the file after
// a read that returned n bytes, possibly none, and records where these bytes
// start in the file: the descriptor's position minus what the buffered reader
// still holds and minus the n bytes themselves.
func (r *positionReporter) readReturned(n int) error {
	if r == nil {
		return nil
	}
	position, err := r.fd.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("read position of %s: %w", r.fd.Name(), err)
	}
	r.readStart = position - int64(r.reader.Buffered()) - int64(n)
	r.observer.ReadUpTo(position, r.file)
	return nil
}

// lineEndsAt reports that the line just fed ends after the first readEnd
// bytes of the current read.
func (r *positionReporter) lineEndsAt(readEnd int) {
	if r == nil {
		return
	}
	r.observer.LineEndsAt(r.readStart+int64(readEnd), r.file)
}

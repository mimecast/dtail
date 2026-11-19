package fs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mimecast/dtail/internal/ctxutil"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
)

const defaultMaxLineLength = 1024 * 1024

var (
	errFileTruncated = errors.New("file got truncated")
	errFileRotated   = errors.New("file got rotated")
)

// ErrStartOffsetFileChanged reports that the file at the path is not the one
// ReadOptions.StartOffset was measured in: the path was rotated in between.
// The reader read nothing; it dropped the start offset, so its next start
// reads the file at the path from the beginning.
var ErrStartOffsetFileChanged = errors.New("file changed since the read start offset was taken")

// ErrHandedOver reports that a follow read ended because
// ReadOptions.HandOverAtEOF took the read over at the end of the file.
var ErrHandedOver = errors.New("follow read handed over at the end of the file")

// ReadOptions configures a file-backed reader.
type ReadOptions struct {
	Mode           omode.Mode
	Target         *ValidatedReadTarget
	FilePath       string
	GlobID         string
	ServerMessages chan<- string
	SeekEOF        bool
	// StartOffset, when positive, positions the first successful open of the
	// file at this byte offset instead of its start. It is meant for resuming a
	// read where another reader of the same file stopped, so it must point at a
	// line boundary of the file StartOffsetFile describes, which is required
	// with it. If the file opened is a different one (the path was rotated in
	// between), Start returns ErrStartOffsetFileChanged without reading, so
	// that the caller can prepare for a new file, and the next start reads the
	// new file from its beginning. Later reopens start at the beginning as
	// usual. If the file is shorter than the offset, a follow read treats it
	// as truncated: it logs so, tells a line.SourceRestarter processor that
	// its source starts over, and rewinds. StartOffset cannot be combined with
	// SeekEOF and is only supported for uncompressed files, not for the stdin
	// pipe.
	StartOffset int64
	// StartOffsetFile identifies the file StartOffset was measured in, as
	// returned by os.File.Stat or os.Stat for it.
	StartOffsetFile os.FileInfo
	// StartOffsetInSplitLine tells a follow reader that StartOffset is where
	// an earlier reader split a line longer than MaxLineLength and warned
	// about it, so that it does not warn again about the rest of that line.
	StartOffsetInSplitLine bool
	// StartFile, when set, is an open descriptor, positioned at its
	// beginning, which the first read uses instead of opening the path, so
	// that it reads that file even if the path was rotated away from it
	// meanwhile: from StartOffset, when positive, in which case it must be
	// the file StartOffsetFile describes, or from its beginning. The reader
	// takes ownership and closes it when that read ends. It cannot be
	// combined with SeekEOF and is only supported for uncompressed files, not
	// for the stdin pipe.
	StartFile *os.File
	// HandOverAtEOF, when set, is asked by a follow read of an uncompressed
	// file whenever it reached the end of the file at the start of a line,
	// having fed every line before it to its filter and flushed the
	// processor, and found the file neither truncated nor rotated: offset is
	// the end of the file, file its identity. When it returns true, the read
	// ends with ErrHandedOver, having fed nothing past offset, and keeps the
	// local context of its filter, so that the caller can go on feeding the
	// same filter (see ReadFile.StartFiltered) from offset on from elsewhere.
	// It runs on the reader's goroutine. It cannot be combined with a
	// compressed file or the stdin pipe.
	HandOverAtEOF func(offset int64, file os.FileInfo) bool
	MaxLineLength int
	Logger        logging.Logger
}

// ReadFile reads and filters a local file or serverless pipe.
type ReadFile struct {
	// Logger for filesystem diagnostics.
	logger logging.Logger
	// Various statistics (e.g. regex hit percentage, transfer percentage).
	stats
	// Path of log file to tail.
	filePath string
	// Rooted target used for validated server-side re-opens. It is an atomic
	// pointer so that ReplaceTarget can hand it over while a read runs.
	validatedTarget atomic.Pointer[ValidatedReadTarget]
	// The glob identifier of the file.
	globID string
	// Channel to send a server message to the dtail client
	serverMessages chan<- string
	// Periodically retry reading file.
	retry bool
	// Can I skip messages when there are too many?
	canSkipLines bool
	// Keep reading when the current file reaches EOF?
	follow bool
	// Seek past existing data on the first successful file open?
	seekInitialEOF bool
	// Byte offset for the first successful file open; zero means the start.
	initialOffset int64
	// The file initialOffset belongs to; the offset only applies to it.
	initialOffsetFile os.FileInfo
	// initialOffset is inside a line a reader already warned about.
	initialOffsetInSplitLine bool
	// initialFile is the open file for the first read, if given.
	initialFile *os.File
	// handOverAtEOF, if set, may end a follow read at the end of the file.
	handOverAtEOF func(offset int64, file os.FileInfo) bool
	// Warned already about a long line.
	warnedAboutLongLine bool
	// Maximum line length before a line is split.
	maxLineLength int
	// pipeInput overrides os.Stdin for serverless pipe reads. It exists so the
	// cancellation and ownership contract can be tested without replacing the
	// process-wide stdin descriptor.
	pipeInput *os.File
	// truncateCheck is an optional test seam for the periodic child goroutine.
	truncateCheck func(context.Context, chan<- struct{})
	// rotationDrainPoll is an optional test seam replacing the wait before
	// each poll of a rotated-away file (see pollRotatedFile).
	rotationDrainPoll func(context.Context) bool
	// bufferRecycleObserver is an optional test seam for asserting buffer
	// ownership without relying on sync.Pool retrieval order.
	bufferRecycleObserver func(*bytes.Buffer)
}

var _ FileReader = (*ReadFile)(nil)

// NewReadFile returns a reader configured for a snapshot or follow operation.
func NewReadFile(options ReadOptions) (*ReadFile, error) {
	retry, canSkipLines, follow, err := readBehavior(options.Mode)
	if err != nil {
		return nil, err
	}

	if err := validateStartOffset(options); err != nil {
		return nil, err
	}

	var target *ValidatedReadTarget
	if options.Target != nil {
		if options.Target.Kind != FileKind {
			return nil, fmt.Errorf("read file requires target kind %d, got %d", FileKind, options.Target.Kind)
		}
		targetCopy := *options.Target
		target = &targetCopy
	}

	readFile := &ReadFile{
		logger:            logging.OrNop(options.Logger),
		filePath:          options.FilePath,
		globID:            options.GlobID,
		serverMessages:    options.ServerMessages,
		retry:             retry,
		canSkipLines:      canSkipLines,
		follow:            follow,
		seekInitialEOF:    options.SeekEOF,
		initialOffset:     options.StartOffset,
		initialOffsetFile: options.StartOffsetFile,
		maxLineLength:     options.MaxLineLength,
		// Only meaningful together with a start offset.
		initialOffsetInSplitLine: options.StartOffsetInSplitLine && options.StartOffset > 0,
		initialFile:              options.StartFile,
		handOverAtEOF:            options.HandOverAtEOF,
	}
	readFile.validatedTarget.Store(target)
	return readFile, nil
}

func validateStartOffset(options ReadOptions) error {
	if err := validateStartFile(options); err != nil {
		return err
	}
	if err := validateHandOver(options); err != nil {
		return err
	}
	switch {
	case options.StartOffset == 0:
		return nil
	case options.StartOffset < 0:
		return fmt.Errorf("negative read start offset %d", options.StartOffset)
	case options.StartOffsetFile == nil:
		return errors.New("read start offset needs the identity of the file it belongs to")
	case options.SeekEOF:
		return errors.New("read start offset and seek to EOF are mutually exclusive")
	case options.FilePath == "" && options.GlobID == "-":
		return errors.New("read start offset is not supported for the stdin pipe")
	case CompressionFormat(options.FilePath) != "":
		return fmt.Errorf("read start offset is not supported for compressed file %s",
			options.FilePath)
	}
	return nil
}

func validateStartFile(options ReadOptions) error {
	switch {
	case options.StartFile == nil:
		return nil
	case options.SeekEOF:
		return errors.New("read start file and seek to EOF are mutually exclusive")
	case options.FilePath == "" && options.GlobID == "-":
		return errors.New("read start file is not supported for the stdin pipe")
	case CompressionFormat(options.FilePath) != "":
		return fmt.Errorf("read start file is not supported for compressed file %s", options.FilePath)
	}
	return nil
}

func validateHandOver(options ReadOptions) error {
	switch {
	case options.HandOverAtEOF == nil:
		return nil
	case options.FilePath == "" && options.GlobID == "-":
		return errors.New("hand-over at the end of the file is not supported for the stdin pipe")
	case CompressionFormat(options.FilePath) != "":
		return fmt.Errorf("hand-over at the end of the file is not supported for compressed file %s",
			options.FilePath)
	}
	return nil
}

func readBehavior(mode omode.Mode) (retry, canSkipLines, follow bool, err error) {
	switch mode {
	case omode.CatClient, omode.GrepClient:
		return false, false, false, nil
	case omode.TailClient:
		return true, true, true, nil
	default:
		return false, false, false, fmt.Errorf("unsupported read mode: %s (%d)", mode, mode)
	}
}

// String returns the string representation of the ReadFile
func (f *ReadFile) String() string {
	return fmt.Sprintf(
		"readFile(filePath:%s,globID:%s,retry:%v,canSkipLines:%v,follow:%v,seekInitialEOF:%v)",
		f.filePath,
		f.globID,
		f.retry,
		f.canSkipLines,
		f.follow,
		f.seekInitialEOF)
}

// ReplaceTarget makes the reader open, reopen and check the file through
// target from now on, for example when the session whose target a shared
// reader used leaves. target must name the same resolved file as the current
// one, which must exist; a reader built without a target keeps none.
func (f *ReadFile) ReplaceTarget(target ValidatedReadTarget) error {
	current := f.validatedTarget.Load()
	switch {
	case current == nil:
		return errors.New("replace read target: reader has no validated target")
	case target.Kind != FileKind:
		return fmt.Errorf("replace read target: requires target kind %d, got %d", FileKind, target.Kind)
	case target.resolvedPath != current.resolvedPath:
		return fmt.Errorf("replace read target: %s does not match %s",
			target.resolvedPath, current.resolvedPath)
	}
	f.validatedTarget.Store(&target)
	return nil
}

// FilePath returns the full file path.
func (f *ReadFile) FilePath() string {
	return f.filePath
}

// Retry reading the file on error?
func (f *ReadFile) Retry() bool {
	return f.retry
}

func (f *ReadFile) lineLimit() int {
	if f.maxLineLength <= 0 {
		return defaultMaxLineLength
	}
	return f.maxLineLength
}

// LongLineWarning formats the message a reader sends to the client, through
// its ServerMessages channel, when it splits a line of filePath because the
// line is longer than the maximum line length.
func LongLineWarning(logger logging.Logger, filePath string) string {
	return logging.OrNop(logger).Warn(filePath, "Long log line, splitting into multiple lines") + "\n"
}

func (f *ReadFile) warnAboutLongLine(ctx context.Context) bool {
	if f.warnedAboutLongLine {
		return true
	}

	if f.serverMessages == nil {
		f.warnedAboutLongLine = true
		return true
	}

	select {
	case f.serverMessages <- LongLineWarning(f.logger, f.filePath):
		f.warnedAboutLongLine = true
		return true
	case <-ctx.Done():
		return false
	}
}

func (f *ReadFile) makeReader(ctx context.Context) (*bufio.Reader, *os.File, io.Closer, error) {
	if f.filePath == "" && f.globID == "-" {
		return f.makePipeReader(ctx)
	}
	return f.makeFileReader()
}

func (f *ReadFile) makeFileReader() (reader *bufio.Reader, fd *os.File, decompressor io.Closer, err error) {
	seekInitialEOF := f.seekInitialEOF
	if f.initialFile != nil {
		fd, f.initialFile = f.initialFile, nil
	} else if fd, err = f.openFile(); err != nil {
		return
	}

	if err = f.seekInitialPosition(fd); err != nil {
		return
	}

	reader, decompressor, err = f.makeCompressedFileReader(fd)
	if err == nil {
		initialOffset := f.initialOffset
		f.seekInitialEOF = false
		f.initialOffset = 0
		f.initialOffsetFile = nil
		f.logger.Trace(f.filePath, f.globID, "Opened file reader", "seekInitialEOF", seekInitialEOF,
			"initialOffset", initialOffset)
	}
	return
}

// seekInitialPosition applies SeekEOF or StartOffset. Both are cleared only once
// a reader was made, so a failed first open keeps them for the retry.
func (f *ReadFile) seekInitialPosition(fd *os.File) error {
	switch {
	case f.seekInitialEOF:
		if _, err := fd.Seek(0, io.SeekEnd); err != nil {
			return fmt.Errorf("seek to end of %s: %w", f.filePath, err)
		}
	case f.initialOffset > 0:
		return f.seekInitialOffset(fd)
	}
	return nil
}

// seekInitialOffset seeks to StartOffset, unless the open file is not the one
// the offset was measured in.
func (f *ReadFile) seekInitialOffset(fd *os.File) error {
	info, err := fd.Stat()
	if err != nil {
		return fmt.Errorf("stat %s for start offset: %w", f.filePath, err)
	}
	if !os.SameFile(info, f.initialOffsetFile) {
		f.initialOffset = 0
		f.initialOffsetFile = nil
		f.initialOffsetInSplitLine = false
		return fmt.Errorf("%s: %w", f.filePath, ErrStartOffsetFileChanged)
	}
	if _, err := fd.Seek(f.initialOffset, io.SeekStart); err != nil {
		return fmt.Errorf("seek to offset %d of %s: %w", f.initialOffset, f.filePath, err)
	}
	// The rest of a split line the earlier reader warned about.
	if f.initialOffsetInSplitLine {
		f.warnedAboutLongLine = true
		f.initialOffsetInSplitLine = false
	}
	return nil
}

func (f *ReadFile) openFile() (*os.File, error) {
	if target := f.validatedTarget.Load(); target != nil {
		return target.Open()
	}
	return os.Open(f.filePath)
}

func (f *ReadFile) makePipeReader(ctx context.Context) (*bufio.Reader, *os.File, io.Closer, error) {
	input := f.pipeInput
	if input == nil {
		input = os.Stdin
	}
	reader, err := newContextFileReader(ctx, input)
	if err != nil {
		return nil, nil, nil, err
	}
	return bufio.NewReader(reader), nil, reader, nil
}

func (f *ReadFile) periodicTruncateCheck(ctx context.Context, truncate chan<- struct{}) {
	ticker := time.NewTicker(time.Second * 3)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			select {
			case truncate <- struct{}{}:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// pollRotatedFile waits before the next poll of a file the path was rotated
// away from, and reports false when ctx ended meanwhile.
func (f *ReadFile) pollRotatedFile(ctx context.Context) bool {
	if f.rotationDrainPoll != nil {
		return f.rotationDrainPoll(ctx)
	}
	return ctxutil.Sleep(ctx, rotationDrainPollInterval)
}

func (f *ReadFile) startPeriodicTruncateCheck(ctx context.Context, cancel context.CancelFunc,
	truncate chan<- struct{}) <-chan error {
	done := make(chan error, 1)
	check := f.periodicTruncateCheck
	if f.truncateCheck != nil {
		check = f.truncateCheck
	}
	go func() {
		var childErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				childErr = fmt.Errorf("%w: truncate checker: %v", ErrReaderWorkerPanic, recovered)
				logging.OrNop(f.logger).Error(f.filePath, childErr, "stack", string(debug.Stack()))
				cancel()
			}
			done <- childErr
		}()
		check(ctx, truncate)
	}()
	return done
}

// CompressionFormat names the decompressor a path is read through, by its
// suffix ("gzip" or "zstd"), or returns "" for an uncompressed file.
func CompressionFormat(path string) string {
	switch {
	case strings.HasSuffix(path, ".gz"), strings.HasSuffix(path, ".gzip"):
		return "gzip"
	case strings.HasSuffix(path, ".zst"):
		return "zstd"
	default:
		return ""
	}
}

func (f *ReadFile) makeCompressedFileReader(fd *os.File) (reader *bufio.Reader, decompressor io.Closer, err error) {
	switch CompressionFormat(f.FilePath()) {
	case "gzip":
		f.logger.Info(f.FilePath(), "Detected gzip compression format")
		var gzipReader *gzip.Reader
		gzipReader, err = gzip.NewReader(fd)
		if err != nil {
			return
		}
		decompressor = gzipReader
		reader = bufio.NewReader(gzipReader)
	case "zstd":
		return f.makeZstdReader(fd)
	default:
		reader = bufio.NewReader(fd)
	}
	return
}

// truncated reports whether the open file was truncated in place or replaced
// at its path. A replacement must be detected by identity, because its size can
// equal or exceed the current read offset. An in-place truncation is only seen
// when the file is still shorter than the read offset at the next check; a
// rewrite that has already grown back to or past that offset goes undetected:
// reading resumes at the old offset of the new content, skipping everything
// before it, and a partial line pending from the old content is joined to the
// first bytes read from the new one.
func (f *ReadFile) truncated(fd *os.File) (bool, error) {
	truncated, _, _, err := f.inspectOpenFile(fd)
	return truncated, err
}

// inspectOpenFile is truncated, and also returns the descriptor's offset in
// the open file and the file's identity, which are only set when the file was
// neither truncated nor replaced.
func (f *ReadFile) inspectOpenFile(fd *os.File) (truncated bool, offset int64, file os.FileInfo, err error) {
	if fd == nil {
		return false, 0, nil, nil
	}

	f.logger.Debug(f.filePath, "File truncation check")

	// Can not seek currently open FD.
	currentPosition, err := fd.Seek(0, io.SeekCurrent)
	if err != nil {
		return true, 0, nil, err
	}
	openInfo, err := fd.Stat()
	if err != nil {
		return true, 0, nil, err
	}

	pathInfo, err := f.pathInfo()
	if err != nil {
		return true, 0, nil, err
	}
	if !os.SameFile(openInfo, pathInfo) {
		return true, 0, nil, errFileRotated
	}
	if currentPosition > pathInfo.Size() {
		return true, 0, nil, errFileTruncated
	}
	return false, currentPosition, openInfo, nil
}

func (f *ReadFile) pathInfo() (os.FileInfo, error) {
	target := f.validatedTarget.Load()
	if target == nil {
		info, err := os.Stat(f.filePath)
		if err != nil {
			return nil, fmt.Errorf("stat read path %s: %w", f.filePath, err)
		}
		return info, nil
	}

	info, err := os.Lstat(target.resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("lstat validated read path %s: %w",
			target.resolvedPath, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("validated read target is no longer a regular file: %s",
			target.resolvedPath)
	}
	return info, nil
}

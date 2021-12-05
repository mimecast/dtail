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
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"

	"github.com/DataDog/zstd"
)

type readStatus int

const (
	abortReading    readStatus = iota
	continueReading readStatus = iota
)

// Used to tail and filter a local log file.
type readFile struct {
	// Various statistics (e.g. regex hit percentage, transfer percentage).
	stats
	// Path of log file to tail.
	filePath string
	// The glob identifier of the file.
	globID string
	// Channel to send a server message to the dtail client
	serverMessages chan<- string
	// Periodically retry reading file.
	retry bool
	// Can I skip messages when there are too many?
	canSkipLines bool
	// Seek to the EOF before processing file?
	seekEOF bool
	// Warned already about a long line.
	warnedAboutLongLine bool
}

// String returns the string representation of the readFile
func (f readFile) String() string {
	return fmt.Sprintf(
		"readFile(filePath:%s,globID:%s,retry:%v,canSkipLines:%v,seekEOF:%v)",
		f.filePath,
		f.globID,
		f.retry,
		f.canSkipLines,
		f.seekEOF)
}

// FilePath returns the full file path.
func (f readFile) FilePath() string {
	return f.filePath
}

// Retry reading the file on error?
func (f readFile) Retry() bool {
	return f.retry
}

// Start tailing a log file.
func (f readFile) Start(ctx context.Context, ltx lcontext.LContext,
	lines chan<- *line.Line, re regex.Regex) error {

	reader, fd, err := f.makeReader()
	if fd != nil {
		defer fd.Close()
	}
	if err != nil {
		return err
	}

	rawLines := make(chan *bytes.Buffer, 100)
	truncate := make(chan struct{})

	readCtx, readCancel := context.WithCancel(ctx)
	var filterWg sync.WaitGroup
	filterWg.Add(1)

	go f.periodicTruncateCheck(ctx, truncate)
	go func() {
		f.filter(ctx, ltx, rawLines, lines, re)
		filterWg.Done()
		// If the filter stopped, make the reader stop too, no need to read
		// more data if there is nothing more the filter wants to filter for!
		// E.g. it could be that we only want to filter N matches but not more.
		readCancel()
	}()

	err = f.read(readCtx, fd, reader, rawLines, truncate)
	close(rawLines)
	// Filter may sends some data still. So wait until it is done here.
	filterWg.Wait()

	return err
}

func (f *readFile) makeReader() (*bufio.Reader, *os.File, error) {
	if f.filePath == "" && f.globID == "-" {
		return f.makePipeReader()
	}
	return f.makeFileReader()
}

func (f *readFile) makeFileReader() (*bufio.Reader, *os.File, error) {
	var reader *bufio.Reader
	fd, err := os.Open(f.filePath)
	if err != nil {
		return reader, fd, err
	}

	if f.seekEOF {
		fd.Seek(0, io.SeekEnd)
	}

	reader, err = f.makeCompressedFileReader(fd)
	if err != nil {
		return reader, fd, err
	}

	return reader, fd, nil
}

func (f *readFile) makePipeReader() (*bufio.Reader, *os.File, error) {
	return bufio.NewReader(os.Stdin), nil, nil
}

func (f *readFile) periodicTruncateCheck(ctx context.Context, truncate chan struct{}) {
	for {
		select {
		case <-time.After(time.Second * 3):
			select {
			case truncate <- struct{}{}:
			case <-ctx.Done():
			}
		case <-ctx.Done():
			return
		}
	}
}

func (f *readFile) makeCompressedFileReader(fd *os.File) (reader *bufio.Reader, err error) {
	switch {
	case strings.HasSuffix(f.FilePath(), ".gz"):
		fallthrough
	case strings.HasSuffix(f.FilePath(), ".gzip"):
		dlog.Common.Info(f.FilePath(), "Detected gzip compression format")
		var gzipReader *gzip.Reader
		gzipReader, err = gzip.NewReader(fd)
		if err != nil {
			return
		}
		reader = bufio.NewReader(gzipReader)
	case strings.HasSuffix(f.FilePath(), ".zst"):
		dlog.Common.Info(f.FilePath(), "Detected zstd compression format")
		reader = bufio.NewReader(zstd.NewReader(fd))
	default:
		reader = bufio.NewReader(fd)
	}
	return
}

func (f *readFile) read(ctx context.Context, fd *os.File, reader *bufio.Reader,
	rawLines chan *bytes.Buffer, truncate <-chan struct{}) error {

	var offset uint64
	message := pool.BytesBuffer.Get().(*bytes.Buffer)

	for {
		b, err := reader.ReadByte()
		if err != nil {
			status, err := f.handleReadError(ctx, err, fd, rawLines, truncate, message)
			if abortReading == status {
				return err
			}
			time.Sleep(time.Millisecond * 100)
			continue
		}

		offset++
		message.WriteByte(b)

		status, newMessage := f.handleReadByte(ctx, b, rawLines, message)
		if status == abortReading {
			return nil
		}
		message = newMessage
	}
}

// Filter log lines matching a given regular expression.
func (f *readFile) filter(ctx context.Context, ltx lcontext.LContext,
	rawLines <-chan *bytes.Buffer, lines chan<- *line.Line, re regex.Regex) {

	// Do we have any kind of local context settings? If so then run the more complex
	// filterWithLContext method.
	if ltx.Has() {
		// We can not skip transmitting any lines to the client with a local
		// grep context specified.
		f.canSkipLines = false
		f.filterWithLContext(ctx, ltx, rawLines, lines, re)
		return
	}

	f.filterWithoutLContext(ctx, rawLines, lines, re)
}

func (f *readFile) transmittable(rawLine *bytes.Buffer, length, capacity int,
	re regex.Regex) (*line.Line, bool) {

	newLine := line.Null()
	if !re.Match(rawLine.Bytes()) {
		f.updateLineNotMatched()
		f.updateLineNotTransmitted()
		return newLine, false
	}
	f.updateLineMatched()

	// Can we actually send more messages, channel capacity reached?
	if f.canSkipLines && length >= capacity {
		f.updateLineNotTransmitted()
		return newLine, false
	}
	f.updateLineTransmitted()

	return line.New(rawLine, f.totalLineCount(), f.transmittedPerc(), f.globID), true
}

// Check wether log file is truncated. Returns nil if not.
func (f *readFile) truncated(fd *os.File) (bool, error) {
	if fd == nil {
		return false, nil
	}

	dlog.Common.Debug(f.filePath, "File truncation check")

	// Can not seek currently open FD.
	currentPosition, err := fd.Seek(0, os.SEEK_CUR)
	if err != nil {
		return true, err
	}
	// Can not open file at original path.
	pathFd, err := os.Open(f.filePath)
	if err != nil {
		return true, err
	}
	defer pathFd.Close()

	// Can not seek file at original path.
	pathPosition, err := pathFd.Seek(0, io.SeekEnd)
	if err != nil {
		return true, err
	}
	if currentPosition > pathPosition {
		return true, errors.New("File got truncated")
	}
	return false, nil
}

// Deal with the scenario that nothing could be read from the fd.
func (f *readFile) handleReadError(ctx context.Context, err error, fd *os.File,
	rawLines chan *bytes.Buffer, truncate <-chan struct{},
	message *bytes.Buffer) (readStatus, error) {

	if err != io.EOF {
		return abortReading, err
	}

	select {
	case <-truncate:
		if isTruncated, err := f.truncated(fd); isTruncated {
			return abortReading, err
		}
	case <-ctx.Done():
		return abortReading, nil
	default:
	}

	if !f.seekEOF {
		dlog.Common.Info(f.FilePath(), "End of file reached")
		if len(message.Bytes()) > 0 {
			select {
			case rawLines <- message:
			case <-ctx.Done():
			}
		}
		return abortReading, nil
	}

	return continueReading, nil
}

// Now process the byte we just read from the fd.
func (f *readFile) handleReadByte(ctx context.Context, b byte,
	rawLines chan *bytes.Buffer, message *bytes.Buffer) (readStatus, *bytes.Buffer) {

	switch b {
	case '\n':
		select {
		case rawLines <- message:
			message = pool.BytesBuffer.Get().(*bytes.Buffer)
			f.warnedAboutLongLine = false
		case <-ctx.Done():
			return abortReading, message
		}
	default:
		if message.Len() >= config.Server.MaxLineLength {
			if !f.warnedAboutLongLine {
				f.serverMessages <- dlog.Common.Warn(f.filePath,
					"Long log line, splitting into multiple lines")
				f.warnedAboutLongLine = true
			}
			message.WriteByte('\n')
			select {
			case rawLines <- message:
				message = pool.BytesBuffer.Get().(*bytes.Buffer)
			case <-ctx.Done():
				return abortReading, message
			}
		}
	}

	return continueReading, message
}

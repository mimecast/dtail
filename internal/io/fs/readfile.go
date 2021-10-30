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

	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"

	"github.com/DataDog/zstd"
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
	lines chan<- line.Line, re regex.Regex) error {

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

func (f readFile) makeReader() (*bufio.Reader, *os.File, error) {
	if f.filePath == "PIPE" && f.globID == "PIPE" {
		return f.makePipeReader()
	}
	return f.makeFileReader()
}

func (f readFile) makeFileReader() (*bufio.Reader, *os.File, error) {
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

func (f readFile) makePipeReader() (*bufio.Reader, *os.File, error) {
	return bufio.NewReader(os.Stdin), nil, nil
}

func (f readFile) periodicTruncateCheck(ctx context.Context, truncate chan struct{}) {
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

func (f readFile) makeCompressedFileReader(fd *os.File) (reader *bufio.Reader, err error) {
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

func (f readFile) read(ctx context.Context, fd *os.File, reader *bufio.Reader,
	rawLines chan *bytes.Buffer, truncate <-chan struct{}) error {

	var offset uint64

	lineLengthThreshold := 1024 * 1024 // 1mb
	warnedAboutLongLine := false
	message := pool.BytesBuffer.Get().(*bytes.Buffer)

	for {
		b, err := reader.ReadByte()

		if err != nil {
			if err != io.EOF {
				return err
			}
			select {
			case <-truncate:
				if isTruncated, err := f.truncated(fd); isTruncated {
					return err
				}
			case <-ctx.Done():
				return nil
			default:
			}
			if !f.seekEOF {
				dlog.Common.Info(f.FilePath(), "End of file reached")
				return nil
			}
			time.Sleep(time.Millisecond * 100)
			continue
		}
		offset++

		switch b {
		case '\n':
			select {
			case rawLines <- message:
				message = pool.BytesBuffer.Get().(*bytes.Buffer)
				//fmt.Printf("%d %d %p\n", message.Len(), message.Cap(), message)
				warnedAboutLongLine = false
			case <-ctx.Done():
				return nil
			}
		default:
			if message.Len() >= lineLengthThreshold {
				if !warnedAboutLongLine {
					f.serverMessages <- dlog.Common.Warn(f.filePath,
						"Long log line, splitting into multiple lines")
					warnedAboutLongLine = true
				}
				message.WriteString("\n")
				select {
				case rawLines <- message:
					message = pool.BytesBuffer.Get().(*bytes.Buffer)
				case <-ctx.Done():
					return nil
				}
			}
			message.WriteByte(b)
		}
	}
}

// Filter log lines matching a given regular expression.
func (f readFile) filter(ctx context.Context, ltx lcontext.LContext,
	rawLines <-chan *bytes.Buffer, lines chan<- line.Line, re regex.Regex) {

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

func (f readFile) filterWithoutLContext(ctx context.Context, rawLines <-chan *bytes.Buffer,
	lines chan<- line.Line, re regex.Regex) {

	for {
		select {
		case line, ok := <-rawLines:
			f.updatePosition()
			if !ok {
				return
			}
			if filteredLine, ok := f.transmittable(line, len(lines), cap(lines), re); ok {
				select {
				case lines <- filteredLine:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// Filter log lines matching a given regular expression, however with local grep context.
func (f readFile) filterWithLContext(ctx context.Context, ltx lcontext.LContext,
	rawLines <-chan *bytes.Buffer, lines chan<- line.Line, re regex.Regex) {

	// Scenario 1: Finish once maxCount hits found
	maxCount := ltx.MaxCount
	processMaxCount := maxCount > 0
	maxReached := false

	// Scenario 2: Print prev. N lines when current line matches.
	before := ltx.BeforeContext
	processBefore := before > 0
	var beforeBuf chan *bytes.Buffer
	if processBefore {
		beforeBuf = make(chan *bytes.Buffer, before)
	}

	// Screnario 3: Print next N lines when current line matches.
	after := 0
	processAfter := ltx.AfterContext > 0

	for lineBytesBuffer := range rawLines {
		f.updatePosition()

		if !re.Match(lineBytesBuffer.Bytes()) {
			f.updateLineNotMatched()

			if processAfter && after > 0 {
				after--
				myLine := line.Line{
					Content:         lineBytesBuffer,
					SourceID:        f.globID,
					Count:           f.totalLineCount(),
					TransmittedPerc: 100,
				}

				select {
				case lines <- myLine:
				case <-ctx.Done():
					return
				}

			} else if processBefore {
				// Keep last num BeforeContext raw messages.
				select {
				case beforeBuf <- lineBytesBuffer:
				default:
					pool.RecycleBytesBuffer(<-beforeBuf)
					beforeBuf <- lineBytesBuffer
				}
			}
			continue
		}

		f.updateLineMatched()

		if processAfter {
			if maxReached {
				return
			}
			after = ltx.AfterContext
		}

		if processBefore {
			i := uint64(len(beforeBuf))
			for {
				select {
				case lineBytesBuffer := <-beforeBuf:
					myLine := line.Line{
						Content:         lineBytesBuffer,
						SourceID:        f.globID,
						Count:           f.totalLineCount() - i,
						TransmittedPerc: 100,
					}
					i--

					select {
					case lines <- myLine:
					case <-ctx.Done():
						return
					}
				default:
					// beforeBuf is now empty.
				}
				if len(beforeBuf) == 0 {
					break
				}
			}
		}

		line := line.Line{
			Content:         lineBytesBuffer,
			SourceID:        f.globID,
			Count:           f.totalLineCount(),
			TransmittedPerc: 100,
		}

		select {
		case lines <- line:
			if processMaxCount {
				maxCount--
				if maxCount == 0 {
					if !processAfter || after == 0 {
						return
					}
					// Unfortunatley we have to continue filter, as there might be more lines to print
					maxReached = true
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (f readFile) transmittable(lineBytesBuffer *bytes.Buffer, length, capacity int,
	re regex.Regex) (line.Line, bool) {

	var read line.Line
	if !re.Match(lineBytesBuffer.Bytes()) {
		f.updateLineNotMatched()
		f.updateLineNotTransmitted()
		return read, false
	}
	f.updateLineMatched()

	// Can we actually send more messages, channel capacity reached?
	if f.canSkipLines && length >= capacity {
		f.updateLineNotTransmitted()
		return read, false
	}
	f.updateLineTransmitted()

	read = line.Line{
		Content:         lineBytesBuffer,
		SourceID:        f.globID,
		Count:           f.totalLineCount(),
		TransmittedPerc: f.transmittedPerc(),
	}
	return read, true
}

// Check wether log file is truncated. Returns nil if not.
func (f readFile) truncated(fd *os.File) (bool, error) {
	if fd == nil {
		return false, nil
	}

	dlog.Common.Debug(f.filePath, "File truncation check")

	// Can not seek currently open FD.
	curPos, err := fd.Seek(0, os.SEEK_CUR)
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
	pathPos, err := pathFd.Seek(0, io.SeekEnd)
	if err != nil {
		return true, err
	}
	if curPos > pathPos {
		return true, errors.New("File got truncated")
	}
	return false, nil
}

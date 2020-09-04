package fs

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/logger"
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
	limiter chan struct{}
}

// String returns the string representation of the readFile
func (f readFile) String() string {
	return fmt.Sprintf("readFile(filePath:%s,globID:%s,retry:%v,canSkipLines:%v,seekEOF:%v)",
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
func (f readFile) Start(ctx context.Context, lines chan<- line.Line, re regex.Regex) error {
	logger.Debug("readFile", f)
	defer func() {
		select {
		case <-f.limiter:
		default:
		}
	}()

	select {
	case f.limiter <- struct{}{}:
	default:
		select {
		case f.serverMessages <- logger.Warn(f.filePath, f.globID, "Server limit reached. Queuing file..."):
		case <-ctx.Done():
			return nil
		}
		f.limiter <- struct{}{}
	}

	fd, err := os.Open(f.filePath)
	if err != nil {
		return err
	}
	defer fd.Close()

	if f.seekEOF {
		fd.Seek(0, io.SeekEnd)
	}

	rawLines := make(chan []byte, 100)
	truncate := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)

	go f.periodicTruncateCheck(ctx, truncate)
	go f.filter(ctx, &wg, rawLines, lines, re)

	err = f.read(ctx, fd, rawLines, truncate)
	close(rawLines)
	wg.Wait()

	return err
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

func (f readFile) makeReader(fd *os.File) (reader *bufio.Reader, err error) {
	switch {
	case strings.HasSuffix(f.FilePath(), ".gz"):
		fallthrough
	case strings.HasSuffix(f.FilePath(), ".gzip"):
		logger.Info(f.FilePath(), "Detected gzip compression format")
		var gzipReader *gzip.Reader
		gzipReader, err = gzip.NewReader(fd)
		if err != nil {
			return
		}
		reader = bufio.NewReader(gzipReader)
	case strings.HasSuffix(f.FilePath(), ".zst"):
		logger.Info(f.FilePath(), "Detected zstd compression format")
		reader = bufio.NewReader(zstd.NewReader(fd))
	default:
		reader = bufio.NewReader(fd)
	}

	return
}

func (f readFile) read(ctx context.Context, fd *os.File, rawLines chan []byte, truncate <-chan struct{}) error {
	var offset uint64

	reader, err := f.makeReader(fd)
	if err != nil {
		return err
	}
	rawLine := make([]byte, 0, 512)

	lineLengthThreshold := 1024 * 1024 // 1mb
	longLineWarning := false

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		select {
		case <-truncate:
			if isTruncated, err := f.truncated(fd); isTruncated {
				return err
			}
			logger.Info(f.filePath, "Current offset", offset)
		default:
		}

		// Read some bytes (max 4k at once as of go 1.12). isPrefix will
		// be set if line does not fit into 4k buffer.
		bytes, isPrefix, err := reader.ReadLine()

		if err != nil {
			// If EOF, sleep a couple of ms and return with nil error.
			// If other error, return with non-nil error.
			if err != io.EOF {
				return err
			}
			if !f.seekEOF {
				logger.Debug(f.FilePath(), "End of file reached")
				return nil
			}
			time.Sleep(time.Millisecond * 100)
			continue
		}

		rawLine = append(rawLine, bytes...)
		offset += uint64(len(bytes))

		if !isPrefix {
			// last LineRead call returned contend until end of line.
			rawLine = append(rawLine, '\n')
			select {
			case rawLines <- rawLine:
			case <-ctx.Done():
				return nil
			}
			rawLine = make([]byte, 0, 512)
			if longLineWarning {
				longLineWarning = false
			}
			continue
		}

		// Last LineRead call could not read content until end of line, buffer
		// was too small. Determine whether we exceed the max line length we
		// want dtail to send to the client at once. Possibly split up log line
		// into multiple log lines.
		if len(rawLine) >= lineLengthThreshold {
			if !longLineWarning {
				f.serverMessages <- logger.Warn(f.filePath, "Long log line, splitting into multiple lines")
				// Only print out one warning per long log line.
				longLineWarning = true
			}
			rawLine = append(rawLine, '\n')
			select {
			case rawLines <- rawLine:
			case <-ctx.Done():
				return nil
			}
			rawLine = make([]byte, 0, 512)
		}
	}
}

// Filter log lines matching a given regular expression.
func (f readFile) filter(ctx context.Context, wg *sync.WaitGroup, rawLines <-chan []byte, lines chan<- line.Line, re regex.Regex) {
	defer wg.Done()

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

func (f readFile) transmittable(lineBytes []byte, length, capacity int, re regex.Regex) (line.Line, bool) {
	var read line.Line

	if !re.Match(lineBytes) {
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
		Content:         lineBytes,
		SourceID:        f.globID,
		Count:           f.totalLineCount(),
		TransmittedPerc: f.transmittedPerc(),
	}

	return read, true
}

// Check wether log file is truncated. Returns nil if not.
func (f readFile) truncated(fd *os.File) (bool, error) {
	logger.Debug(f.filePath, "File truncation check")

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

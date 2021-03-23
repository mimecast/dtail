package fs

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/logger"
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
	limiter chan struct{}
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
func (f readFile) Start(ctx context.Context, lContext lcontext.LContext, lines chan<- line.Line, re regex.Regex) error {
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
	readCtx, readCancel := context.WithCancel(ctx)

	filterDone := make(chan struct{})
	go func() {
		f.filter(ctx, rawLines, lines, re, lContext)
		close(filterDone)
		// If the filter stopped, make the reader stop too, no need to read
		// more data if there is nothing more the filter wants to filter for!
		// E.g. it could be that we only want to filter N matches but not more.
		readCancel()
	}()

	err = f.read(readCtx, fd, rawLines)
	close(rawLines)

	// Filter may flushes some data still. So wait until it is done here.
	<-filterDone

	return err
}

func (f readFile) read(ctx context.Context, fd *os.File, rawLines chan []byte) error {
	var offset uint64

	reader, err := f.makeReader(fd)
	if err != nil {
		return err
	}
	rawLine := make([]byte, 0, 512)

	lineLengthThreshold := 1024 * 1024 // 1mb
	longLineWarning := false

	checkTruncate := f.truncateTimer(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		select {
		case <-checkTruncate:
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

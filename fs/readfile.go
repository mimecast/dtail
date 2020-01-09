package fs

import (
	"bufio"
	"compress/gzip"
	"dtail/logger"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/DataDog/zstd"
)

// Used to tail and filter a local log file.
type readFile struct {
	// Various statistics (e.g. regex hit percentage, transfer percentage).
	stats
	// Path of log file to tail.
	filePath string
	// Only consider all log lines matching this regular expression.
	re *regexp.Regexp
	// The glob identifier of the file.
	globID string
	// Channel to send a server message to the dtail client
	serverMessages chan<- string
	// Signals to stop tailing the log file.
	stop chan struct{}
	// Periodically retry reading file.
	retry bool
	// Can I skip messages when there are too many?
	canSkipLines bool
	// Seek to the EOF before processing file?
	seekEOF bool
	// Mutex to control the stopping of the file
	mutex   *sync.Mutex
	limiter chan struct{}
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
func (f readFile) Start(lines chan<- LineRead, regex string) error {
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
		case <-f.stop:
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

	go f.periodicTruncateCheck(truncate)
	go f.filter(&wg, rawLines, lines, regex)

	err = f.read(fd, rawLines, truncate)
	close(rawLines)
	wg.Wait()

	return err
}

func (f readFile) periodicTruncateCheck(truncate chan struct{}) {
	for {
		select {
		case <-time.After(time.Second * 3):
			select {
			case truncate <- struct{}{}:
			case <-f.stop:
			}
		case <-f.stop:
			return
		}
	}
}

// Stop reading file.
func (f readFile) Stop() {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	select {
	case <-f.stop:
		return
	default:
	}

	close(f.stop)
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

func (f readFile) read(fd *os.File, rawLines chan []byte, truncate <-chan struct{}) error {
	reader, err := f.makeReader(fd)
	if err != nil {
		return err
	}
	rawLine := make([]byte, 0, 512)
	var offset uint64

	lineLengthThreshold := 1024 * 1024 // 1mb
	longLineWarning := false

	for {
		select {
		case <-truncate:
			if isTruncated, err := f.truncated(fd); isTruncated {
				return err
			}
			logger.Info(f.filePath, "Current offset", offset)

		case <-f.stop:
			return nil
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
			case <-f.stop:
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
			case <-f.stop:
				return nil
			}
			rawLine = make([]byte, 0, 512)
		}
	}
}

// Filter log lines matching a given regular expression.
func (f readFile) filter(wg *sync.WaitGroup, rawLines <-chan []byte, lines chan<- LineRead, regex string) {
	defer wg.Done()

	if regex == "" {
		regex = "."
	}

	re, err := regexp.Compile(regex)
	if err != nil {
		logger.Error(regex, "Can't compile regex, using '.' instead", err)
		re = regexp.MustCompile(".")
	}
	f.re = re

	for {
		select {
		case line, ok := <-rawLines:
			f.updatePosition()
			if !ok {
				return
			}
			if filteredLine, ok := f.transmittable(line, len(lines), cap(lines)); ok {
				select {
				case lines <- filteredLine:
				case <-f.stop:
					return
				}
			}
		}
	}
}

func (f readFile) transmittable(line []byte, length, capacity int) (LineRead, bool) {
	var read LineRead

	if !f.re.Match(line) {
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

	read = LineRead{
		Content:         line,
		GlobID:          &f.globID,
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

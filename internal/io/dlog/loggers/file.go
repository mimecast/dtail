package loggers

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

type fileMessageBuf struct {
	now     time.Time
	message string
}

type file struct {
	bufferCh    chan *fileMessageBuf
	pauseCh     chan struct{}
	resumeCh    chan struct{}
	rotateCh    chan struct{}
	flushCh     chan struct{}
	lastDateStr string
	fd          *os.File
	writer      *bufio.Writer
	mutex       sync.Mutex
	started     bool
}

func newFile() *file {
	f := file{
		bufferCh: make(chan *fileMessageBuf, runtime.NumCPU()*100),
		pauseCh:  make(chan struct{}),
		resumeCh: make(chan struct{}),
		rotateCh: make(chan struct{}),
		flushCh:  make(chan struct{}),
	}
	f.getWriter(time.Now().Format("20060102"))
	return &f
}

func (s *file) Start(ctx context.Context, wg *sync.WaitGroup) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Logger already started from another Goroutine.
	if s.started {
		wg.Done()
		return
	}

	pause := func(ctx context.Context) {
		select {
		case <-s.resumeCh:
			return
		case <-ctx.Done():
			return
		}
	}

	go func() {
		defer wg.Done()

		for {
			select {
			case m := <-s.bufferCh:
				s.write(m)
			case <-s.pauseCh:
				pause(ctx)
			case <-s.flushCh:
				s.flush()
			case <-ctx.Done():
				s.flush()
				s.fd.Close()
				return
			}
		}
	}()

	s.started = true
}

func (s *file) Log(now time.Time, message string) {
	s.bufferCh <- &fileMessageBuf{now, message}
}

func (s *file) LogWithColors(now time.Time, message, coloredMessage string) {
	panic("Colors not supported in file logger")
}

func (s *file) Pause()  { s.pauseCh <- struct{}{} }
func (s *file) Resume() { s.resumeCh <- struct{}{} }
func (s *file) Flush()  { s.flushCh <- struct{}{} }

// TODO: Test that Rotate() actually works.
func (s *file) Rotate()           { s.rotateCh <- struct{}{} }
func (file) SupportsColors() bool { return false }

func (s *file) write(m *fileMessageBuf) {
	select {
	case <-s.rotateCh:
		// Force re-opening the outfile.
		s.lastDateStr = ""
	default:
	}

	writer := s.getWriter(m.now.Format("20060102"))
	writer.WriteString(m.message)
	writer.WriteByte('\n')
}

func (s *file) getWriter(dateStr string) *bufio.Writer {
	if s.lastDateStr == dateStr {
		return s.writer
	}

	if _, err := os.Stat(config.Common.LogDir); os.IsNotExist(err) {
		if err = os.MkdirAll(config.Common.LogDir, 0755); err != nil {
			panic(err)
		}
	}

	logFile := fmt.Sprintf("%s/%s.log", config.Common.LogDir, dateStr)
	newFd, err := os.OpenFile(logFile, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		panic(err)
	}

	// Close old writer.
	if s.fd != nil {
		s.writer.Flush()
		s.fd.Close()
	}

	s.fd = newFd
	s.writer = bufio.NewWriterSize(s.fd, 1)
	s.lastDateStr = dateStr

	return s.writer
}

func (s *file) flush() {
	defer s.writer.Flush()

	for {
		select {
		case m := <-s.bufferCh:
			s.write(m)
		default:
			return
		}
	}
}

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

type fileWriter struct {
}

type fileMessageBuf struct {
	now     time.Time
	message string
}

type file struct {
	bufferCh     chan *fileMessageBuf
	pauseCh      chan struct{}
	resumeCh     chan struct{}
	rotateCh     chan struct{}
	flushCh      chan struct{}
	fd           *os.File
	writer       *bufio.Writer
	mutex        sync.Mutex
	started      bool
	lastFileName string
	strategy     Strategy
}

func newFile(strategy Strategy) *file {
	f := file{
		bufferCh: make(chan *fileMessageBuf, runtime.NumCPU()*100),
		pauseCh:  make(chan struct{}),
		resumeCh: make(chan struct{}),
		rotateCh: make(chan struct{}),
		flushCh:  make(chan struct{}),
		strategy: strategy,
	}

	return &f
}

func (f *file) Start(ctx context.Context, wg *sync.WaitGroup) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	// Logger already started from another Goroutine.
	if f.started {
		wg.Done()
		return
	}

	pause := func(ctx context.Context) {
		select {
		case <-f.resumeCh:
			return
		case <-ctx.Done():
			return
		}
	}

	go func() {
		defer wg.Done()

		for {
			select {
			case m := <-f.bufferCh:
				f.write(m)
			case <-f.pauseCh:
				pause(ctx)
			case <-f.flushCh:
				f.flush()
			case <-ctx.Done():
				f.flush()
				f.fd.Close()
				return
			}
		}
	}()

	f.started = true
}

func (f *file) Log(now time.Time, message string) {
	f.bufferCh <- &fileMessageBuf{now, message}
}

func (f *file) LogWithColors(now time.Time, message, coloredMessage string) {
	panic("Colors not supported in file logger")
}

func (f *file) Pause()  { f.pauseCh <- struct{}{} }
func (f *file) Resume() { f.resumeCh <- struct{}{} }
func (f *file) Flush()  { f.flushCh <- struct{}{} }

func (f *file) Rotate()            { f.rotateCh <- struct{}{} }
func (*file) SupportsColors() bool { return false }

func (f *file) write(m *fileMessageBuf) {
	select {
	case <-f.rotateCh:
		// Force re-opening the outfile next time in getWriter.
		f.lastFileName = ""
	default:
	}

	var writer *bufio.Writer
	if f.strategy.Rotation == DailyRotation {
		writer = f.getWriter(m.now.Format("20060102"))
	} else {
		writer = f.getWriter(f.strategy.FileBase)
	}

	writer.WriteString(m.message)
	writer.WriteByte('\n')
}

func (f *file) getWriter(name string) *bufio.Writer {
	if f.lastFileName == name {
		return f.writer
	}
	if _, err := os.Stat(config.Common.LogDir); os.IsNotExist(err) {
		if err = os.MkdirAll(config.Common.LogDir, 0755); err != nil {
			panic(err)
		}
	}

	logFile := fmt.Sprintf("%s/%s.log", config.Common.LogDir, name)
	newFd, err := os.OpenFile(logFile, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		panic(err)
	}

	// Close old writer.
	if f.fd != nil {
		f.writer.Flush()
		f.fd.Close()
	}
	// Set new writer.
	f.fd = newFd
	f.writer = bufio.NewWriterSize(f.fd, 1)
	f.lastFileName = name

	return f.writer
}

func (f *file) flush() {
	defer func() {
		if f.writer != nil {
			f.writer.Flush()
		}
	}()
	for {
		select {
		case m := <-f.bufferCh:
			f.write(m)
		default:
			return
		}
	}
}

package loggers

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type stdout struct {
	pauseCh  chan struct{}
	resumeCh chan struct{}
	mutex    sync.Mutex
}

func newStdout() *stdout {
	return &stdout{
		pauseCh:  make(chan struct{}),
		resumeCh: make(chan struct{}),
	}
}

func (s *stdout) Start(ctx context.Context, wg *sync.WaitGroup) {
	wg.Done()
}

func (s *stdout) Log(now time.Time, message string) {
	s.log(message, true)
}

func (s *stdout) LogWithColors(now time.Time, message, coloredMessage string) {
	s.log(coloredMessage, true)
}

func (s *stdout) Raw(now time.Time, message string) {
	s.log(message, false)
}

func (s *stdout) RawWithColors(now time.Time, message, coloredMessage string) {
	s.log(coloredMessage, false)
}

func (s *stdout) log(message string, nl bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	select {
	case <-s.pauseCh:
		// Pause until resumed.
		<-s.resumeCh
	default:
	}

	if nl {
		fmt.Println(message)
		return
	}
	fmt.Print(message)
}

func (s *stdout) Pause()  { s.pauseCh <- struct{}{} }
func (s *stdout) Resume() { s.resumeCh <- struct{}{} }

func (s *stdout) Flush() {
	// This is empty because it isn't doing anything but has to satisfy the interface.
}

func (s *stdout) Rotate() {
	// This is empty because it isn't doing anything but has to satisfy the interface.
}

func (*stdout) SupportsColors() bool { return true }

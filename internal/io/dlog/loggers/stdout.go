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
	s.log(message)
}

func (s *stdout) LogWithColors(now time.Time, message, coloredMessage string) {
	s.log(coloredMessage)
}

func (s *stdout) log(message string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	select {
	case <-s.pauseCh:
		// Pause until resumed.
		<-s.resumeCh
	default:
	}

	fmt.Println(message)
}

func (s *stdout) Pause()  { s.pauseCh <- struct{}{} }
func (s *stdout) Resume() { s.resumeCh <- struct{}{} }
func (s *stdout) Flush()  {}
func (s *stdout) Rotate() {}

func (stdout) SupportsColors() bool { return true }

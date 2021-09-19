package loggers

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type stdout struct {
	bufferCh chan string
	pauseCh  chan struct{}
	resumeCh chan struct{}
}

func newStdout() *stdout {
	return &stdout{
		bufferCh: make(chan string, 100),
		pauseCh:  make(chan struct{}),
		resumeCh: make(chan struct{}),
	}
}

func (s *stdout) Start(ctx context.Context, wg *sync.WaitGroup) {
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
			case message := <-s.bufferCh:
				fmt.Println(message)
			case <-s.pauseCh:
				pause(ctx)
			case <-ctx.Done():
				s.Flush()
				return
			}
		}
	}()
}

func (s *stdout) Log(now time.Time, message string) {
	s.bufferCh <- message
}

func (s *stdout) LogWithColors(now time.Time, message, coloredMessage string) {
	s.bufferCh <- coloredMessage
}

func (s *stdout) Flush() {
	for {
		select {
		case message := <-s.bufferCh:
			fmt.Println(message)
		default:
			return
		}
	}
}

func (s *stdout) Pause()            { s.pauseCh <- struct{}{} }
func (s *stdout) Resume()           { s.resumeCh <- struct{}{} }
func (s *stdout) Rotate()           {}
func (stdout) SupportsColors() bool { return true }

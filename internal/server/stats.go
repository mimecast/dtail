package server

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/logger"
)

// Used to collect and display various server stats.
type stats struct {
	mutex               sync.Mutex
	currentConnections  int
	lifetimeConnections uint64
}

func (s *stats) incrementConnections() {
	defer s.logServerStats()

	s.mutex.Lock()
	s.currentConnections++
	s.lifetimeConnections++
	s.mutex.Unlock()
}

func (s *stats) decrementConnections() {
	defer s.logServerStats()

	s.mutex.Lock()
	s.currentConnections--
	s.mutex.Unlock()
}

func (s *stats) hasConnections() bool {
	s.mutex.Lock()
	currentConnections := s.currentConnections
	s.mutex.Unlock()

	has := currentConnections > 0
	logger.Info("stats", "Server with open connections?", has, currentConnections)

	return has
}

func (s *stats) logServerStats() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	currentConnections := fmt.Sprintf("currentConnections=%d", s.currentConnections)
	lifetimeConnections := fmt.Sprintf("lifetimeConnections=%d", s.lifetimeConnections)
	goroutines := fmt.Sprintf("goroutines=%d", runtime.NumGoroutine())
	logger.Info("stats", currentConnections, lifetimeConnections, goroutines)
}

func (s *stats) serverLimitExceeded() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.currentConnections >= config.Server.MaxConnections {
		return fmt.Errorf("Exceeded max allowed concurrent connections of %d", config.Server.MaxConnections)
	}

	return nil
}

func (s *stats) start(ctx context.Context) {
	for {
		select {
		case <-time.NewTimer(time.Second * 10).C:
			s.logServerStats()
		case <-ctx.Done():
			return
		}
	}
}

func (s *stats) waitForConnections() {
	for {
		select {
		case <-time.NewTimer(time.Second).C:
			if !s.hasConnections() {
				return
			}
		}
	}
}

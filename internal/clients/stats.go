package clients

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/logger"
)

// Used to collect and display various client stats.
type stats struct {
	// Total amount servers to connect to.
	connectionsTotal int
	// To keep track of what connected and disconnected
	connectionsEstCh chan struct{}
	// Amount of servers connections are established.
	connected int
	// To synchronize concurrent access.
	mutex sync.Mutex
}

func newTailStats(connectionsTotal int) *stats {
	return &stats{
		connectionsTotal: connectionsTotal,
		connectionsEstCh: make(chan struct{}, connectionsTotal),
		connected:        0,
	}
}

// Start starts printing client connection stats every time a signal is recieved or
// connection count has changed.
func (s *stats) Start(ctx context.Context, throttleCh <-chan struct{}, statsCh <-chan string, quiet bool) {
	var connectedLast int

	for {
		var force bool
		var messages []string

		select {
		case message := <-statsCh:
			messages = append(messages, message)
			force = true
		case <-time.After(time.Second * 3):
		case <-ctx.Done():
			return
		}

		connected := len(s.connectionsEstCh)
		throttle := len(throttleCh)

		newConnections := connected - connectedLast
		if (connected == connectedLast || quiet) && !force {
			continue
		}

		stats := s.statsLine(connected, newConnections, throttle)
		switch force {
		case true:
			messages = append(messages, fmt.Sprintf("Connection stats: %s", stats))
			s.printStatsDueInterrupt(messages)
		default:
			logger.Info(stats)
		}

		connectedLast = connected
		s.mutex.Lock()
		s.connected = connected
		s.mutex.Unlock()
	}
}

func (s *stats) printStatsDueInterrupt(messages []string) {
	logger.Pause()
	for i, message := range messages {
		if i > 0 && config.Client.TermColorsEnable {
			fmt.Println(color.PaintStrWithAttr(message,
				config.Client.TermColors.Client.ClientFg,
				config.Client.TermColors.Client.ClientBg,
				config.Client.TermColors.Client.ClientAttr,
			))
			continue
		}
		fmt.Println(fmt.Sprintf(" %s", message))
	}
	time.Sleep(time.Second * time.Duration(config.InterruptTimeoutS))
	logger.Resume()
}

func (s *stats) statsLine(connected, newConnections int, throttle int) string {
	percConnected := percentOf(float64(s.connectionsTotal), float64(connected))

	var stats []string
	stats = append(stats, fmt.Sprintf("connected=%d/%d(%d%%)", connected, s.connectionsTotal, int(percConnected)))
	stats = append(stats, fmt.Sprintf("new=%d", newConnections))
	stats = append(stats, fmt.Sprintf("throttle=%d", throttle))
	stats = append(stats, fmt.Sprintf("cpus/goroutines=%d/%d", runtime.NumCPU(), runtime.NumGoroutine()))

	return strings.Join(stats, "|")
}

func (s *stats) numConnected() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.connected
}

func percentOf(total float64, value float64) float64 {
	if total == 0 || total == value {
		return 100
	}
	return value / (total / 100.0)
}

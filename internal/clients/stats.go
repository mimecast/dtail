package clients

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/protocol"
)

// Used to collect and display various client stats.
type stats struct {
	// Total amount servers to connect to.
	servers int
	// To keep track of what connected and disconnected
	connectionsEstCh chan struct{}
	// Amount of servers connections are established.
	connected int
	// To synchronize concurrent access.
	mutex sync.Mutex
	// Formats interrupt-driven stats output.
	formatter interruptMessageFormatter
	// Controls how long interrupt output remains visible.
	interruptPause time.Duration
	logger         logging.Logger
}

func newTailStats(servers int, formatter interruptMessageFormatter, interruptPause time.Duration,
	logger logging.Logger) *stats {
	if interruptPause <= 0 {
		interruptPause = 3 * time.Second
	}
	return &stats{
		servers:          servers,
		connectionsEstCh: make(chan struct{}, servers),
		connected:        0,
		formatter:        formatter,
		interruptPause:   interruptPause,
		logger:           logging.OrNop(logger),
	}
}

// Start starts printing client connection stats every time a signal is received or
// connection count has changed.
func (s *stats) Start(ctx context.Context, throttleCh <-chan struct{},
	statsCh <-chan string, quiet bool) {

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	var connectedLast int
	for {
		var force bool
		var messages []string

		select {
		case message := <-statsCh:
			messages = append(messages, message)
			force = true
		case <-ticker.C:
		case <-ctx.Done():
			return
		}

		connected := len(s.connectionsEstCh)
		throttle := len(throttleCh)

		newConnections := connected - connectedLast
		if (connected == connectedLast || quiet) && !force {
			continue
		}

		switch force {
		case true:
			stats := s.statsLine(connected, newConnections, throttle)
			messages = append(messages, fmt.Sprintf("Connection stats: %s", stats))
			s.printStatsDueInterrupt(messages)
		default:
			data := s.statsData(connected, newConnections, throttle)
			clientlog.Mapreduce(s.logger, "STATS", data)
		}

		connectedLast = connected
		s.mutex.Lock()
		s.connected = connected
		s.mutex.Unlock()
	}
}

func (s *stats) printStatsDueInterrupt(messages []string) {
	clientlog.Pause(s.logger)
	for i, message := range messages {
		if s.formatter != nil {
			fmt.Println(s.formatter.FormatInterruptMessage(i, message))
			continue
		}
		fmt.Printf(" %s\n", message)
	}
	time.Sleep(s.interruptPause)
	clientlog.Resume(s.logger)
}

func (s *stats) statsData(connected, newConnections int,
	throttle int) map[string]any {

	percConnected := percentOf(float64(s.servers), float64(connected))

	data := make(map[string]any)
	data["connected"] = connected
	data["servers"] = s.servers
	data["connected%"] = int(percConnected)
	data["new"] = newConnections
	data["throttle"] = throttle
	data["goroutines"] = runtime.NumGoroutine()
	data["cgocalls"] = runtime.NumCgoCall()
	data["cpu"] = runtime.NumCPU()

	return data
}

func (s *stats) statsLine(connected, newConnections int, throttle int) string {
	return protocol.EncodeStatsLine(s.statsData(connected, newConnections, throttle))
}

func percentOf(total float64, value float64) float64 {
	if total == 0 || total == value {
		return 100
	}
	return value / (total / 100.0)
}

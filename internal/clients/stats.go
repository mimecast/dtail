package clients

import (
	"context"
	"fmt"
	"runtime"
	"sync"

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

func (s *stats) logStatsOnSignal(ctx context.Context, throttleCh chan struct{}, sigCh chan struct{}) {
	var connectedLast int

	for {
		select {
		case <-sigCh:
		case <-ctx.Done():
			return
		}

		connected := len(s.connectionsEstCh)
		throttle := len(throttleCh)

		newConnections := connected - connectedLast
		s.log(connected, newConnections, throttle)

		s.mutex.Lock()
		defer s.mutex.Unlock()

		connectedLast = connected
		s.connected = connected
	}
}

func (s *stats) log(connected, newConnections int, throttle int) {
	percConnected := percentOf(float64(s.connectionsTotal), float64(connected))

	connectedStr := fmt.Sprintf("connected=%d/%d(%d%%)", connected, s.connectionsTotal, int(percConnected))
	newConnStr := fmt.Sprintf("new=%d", newConnections)
	throttleStr := fmt.Sprintf("throttle=%d", throttle)
	cpusGoroutinesStr := fmt.Sprintf("cpus/goroutines=%d/%d", runtime.NumCPU(), runtime.NumGoroutine())

	logger.Info("stats", connectedStr, newConnStr, throttleStr, cpusGoroutinesStr)
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

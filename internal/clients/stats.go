package clients

import (
	"github.com/mimecast/dtail/internal/logger"
	"fmt"
	"runtime"
	"sync"
	"time"
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

func (s *stats) periodicLogStats(throttleCh chan struct{}, stop <-chan struct{}) {
	connectedLast := 0
	statsInterval := 5

	for {
		select {
		case <-time.After(time.Second * time.Duration(statsInterval)):
		case <-stop:
			return
		}

		connected := len(s.connectionsEstCh)
		throttle := len(throttleCh)

		newConnections := connected - connectedLast
		connectionsPerSecond := float64(newConnections) / float64(statsInterval)
		s.log(connected, newConnections, connectionsPerSecond, throttle)

		connectedLast = connected

		s.mutex.Lock()
		s.connected = connected
		s.mutex.Unlock()
	}
}

func (s *stats) numConnected() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	return s.connected
}

func (s *stats) log(connected, newConnections int, connectionsPerSecond float64, throttle int) {
	percConnected := percentOf(float64(s.connectionsTotal), float64(connected))

	connectedStr := fmt.Sprintf("connected=%d/%d(%d%%)", connected, s.connectionsTotal, int(percConnected))
	newConnStr := fmt.Sprintf("new=%d", newConnections)
	rateStr := fmt.Sprintf("rate=%2.2f/s", connectionsPerSecond)
	throttleStr := fmt.Sprintf("throttle=%d", throttle)
	cpusGoroutinesStr := fmt.Sprintf("cpus/goroutines=%d/%d", runtime.NumCPU(), runtime.NumGoroutine())

	logger.Info("stats", connectedStr, newConnStr, rateStr, throttleStr, cpusGoroutinesStr)
}

func percentOf(total float64, value float64) float64 {
	if total == 0 || total == value {
		return 100
	}
	return value / (total / 100.0)
}

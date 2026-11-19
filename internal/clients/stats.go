package clients

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
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
	// interruptOut is where the interrupt statistics go, set while an
	// interactive session's line editor runs (setInterruptOutput); unset,
	// they go to stdout.
	interruptOut atomic.Pointer[io.Writer]
	logger       logging.Logger
}

// setInterruptOutput makes printStatsDueInterrupt write to w, or to stdout
// again for nil. The interactive control loop sets its line editor's writer
// (the control terminal, the one Ctrl-C was pressed on), which takes the
// line being edited off the screen around the statistics and draws it
// again below them.
func (s *stats) setInterruptOutput(w io.Writer) {
	if w == nil {
		s.interruptOut.Store(nil)
		return
	}
	s.interruptOut.Store(&w)
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

	// The periodic stats are logged by a goroutine of their own, so that this
	// loop never blocks on a paused client logger (an interactive session
	// paused with an empty line, see interactivePause). Blocked here, it no
	// longer received from statsCh, and the interrupt handler then dropped
	// every Ctrl-C as "stats already printed" instead of exiting on the
	// second one.
	logCh := make(chan map[string]any, 1)
	go s.logStats(ctx, logCh)

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
			queueLatestStats(logCh, s.statsData(connected, newConnections, throttle))
		}

		connectedLast = connected
		s.mutex.Lock()
		s.connected = connected
		s.mutex.Unlock()
	}
}

// logStats logs the stats queued by Start until ctx is done.
func (s *stats) logStats(ctx context.Context, logCh <-chan map[string]any) {
	for {
		select {
		case data := <-logCh:
			clientlog.Mapreduce(s.logger, "STATS", data)
		case <-ctx.Done():
			return
		}
	}
}

// queueLatestStats queues data for logStats without blocking. Stats still
// waiting to be logged (the logger is paused) are replaced, as only the
// latest state is of interest. Start is the only sender, so the loop ends
// after at most one replacement.
func queueLatestStats(logCh chan map[string]any, data map[string]any) {
	for {
		select {
		case logCh <- data:
			return
		default:
		}
		select {
		case <-logCh:
		default:
		}
	}
}

// printStatsDueInterrupt prints the statistics asked for with Ctrl-C to
// stdout, or to the interactive line editor (setInterruptOutput), and keeps
// the client logger paused for interruptPause so that they stay visible.
func (s *stats) printStatsDueInterrupt(messages []string) {
	var out io.Writer = os.Stdout
	if w := s.interruptOut.Load(); w != nil {
		out = *w
	}
	clientlog.Pause(s.logger)
	for i, message := range messages {
		if s.formatter != nil {
			_, _ = fmt.Fprintln(out, s.formatter.FormatInterruptMessage(i, message))
			continue
		}
		_, _ = fmt.Fprintf(out, " %s\n", message)
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

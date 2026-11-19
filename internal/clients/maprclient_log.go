package clients

import (
	"sync"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
)

// deferredLogMax is the number of diagnostics a deferredLogger keeps while
// its target does not take them; further ones are dropped and counted.
const deferredLogMax = 256

// deferredLogger passes diagnostics on to target without ever blocking its
// caller: they are queued and logged, in order, by a goroutine that runs
// only while something is queued.
//
// The client's mapreduce state logs through it (newMaprSession), because
// it logs with locks held: writing an outfile logs at INFO under the global
// group set's lock and the session state's read lock (the reporter,
// MaprClient.writeResultsToOutfile). The client
// logger blocks while the terminal output is paused, and the host key
// question pauses it until the interactive control loop passes the answer
// on. A question coming up while the control loop ran a :reload (a server
// still in its SSH handshake) therefore deadlocked the client for good: the
// reporter waited in the paused logger with both locks held, the reload's
// commit and its end (maprclient.SessionState.CommitQuery, Switching) for
// those locks, and the question for the control loop. None of the 10 s caps
// of a reload bounds a lock. (CommitQuery parses the query before it takes
// the write lock; it is the reporter's read lock that held it up.)
//
// The control loop itself must not block in the paused logger either,
// lock or not: what it prints while it commits a reload goes through this
// logger too (MaprClient.controlLogger), payload included (Raw: the last
// table of a query that :tail or a reload ends).
//
// Only the order among the lines logged through it is kept; other lines of
// the same logger may come in between. More than deferredLogMax queued
// diagnostics (a long pause) are dropped and reported by one WARN with
// their number; payload (Raw, RawLog) is never dropped, it is only written
// by the control loop, a few lines per command. Trace diagnostics are not deferred (their arguments
// are formatted late and may change meanwhile); trace logging is a
// debugging aid and may still block under a paused output.
type deferredLogger struct {
	target clientlog.Logger

	mu sync.Mutex
	// drained is signaled when the queue ran empty (flush).
	drained  *sync.Cond
	queue    []func()
	dropped  int
	draining bool
}

var _ clientlog.Logger = (*deferredLogger)(nil)

// newDeferredLogger returns a logger that passes diagnostics on to target.
func newDeferredLogger(target clientlog.Logger) *deferredLogger {
	l := &deferredLogger{target: clientlog.OrNop(target)}
	l.drained = sync.NewCond(&l.mu)
	return l
}

// newMaprSession returns the mapreduce session state of a client, logging
// to logger through a deferredLogger, which it returns as well: whoever
// logs with a lock of the state held must use it, and the client flushes
// it before it ends (MaprClient.flushResultLog).
func newMaprSession(query *mapr.Query, logger clientlog.Logger) (*maprclient.SessionState, *deferredLogger) {
	deferred := newDeferredLogger(logger)
	return maprclient.NewSessionState(query, deferred), deferred
}

// Error logs args at ERROR once the target takes it.
func (l *deferredLogger) Error(args ...any) string {
	l.enqueue(func() { l.target.Error(args...) })
	return ""
}

// Warn logs args at WARN once the target takes it.
func (l *deferredLogger) Warn(args ...any) string {
	l.enqueue(func() { l.target.Warn(args...) })
	return ""
}

// Info logs args at INFO once the target takes it.
func (l *deferredLogger) Info(args ...any) string {
	l.enqueue(func() { l.target.Info(args...) })
	return ""
}

// Debug logs args at DEBUG once the target takes it; nothing is queued
// while debug logging is off.
func (l *deferredLogger) Debug(args ...any) string {
	if l.DebugEnabled() {
		l.enqueue(func() { l.target.Debug(args...) })
	}
	return ""
}

// Raw writes payload once the target takes it; it is never dropped.
func (l *deferredLogger) Raw(message string) string {
	l.enqueuePayload(func() { clientlog.Raw(l.target, message) })
	return ""
}

// RawLog writes a preformatted diagnostic once the target takes it; it is
// never dropped.
func (l *deferredLogger) RawLog(message string) string {
	l.enqueuePayload(func() { clientlog.RawDiagnostic(l.target, message) })
	return ""
}

// Trace logs args at TRACE right away (see deferredLogger).
func (l *deferredLogger) Trace(args ...any) string {
	return l.target.Trace(args...)
}

// TraceEnabled reports whether the target logs at TRACE.
func (l *deferredLogger) TraceEnabled() bool { return l.target.TraceEnabled() }

// DebugEnabled reports whether the target logs at DEBUG.
func (l *deferredLogger) DebugEnabled() bool { return logging.DebugEnabled(l.target) }

// enqueue queues one diagnostic and starts the goroutine logging the queue
// unless it runs. It never blocks on the target.
func (l *deferredLogger) enqueue(log func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queue) >= deferredLogMax {
		l.dropped++
		return
	}
	l.append(log)
}

// enqueuePayload is enqueue for payload, which the queue's bound does not
// drop.
func (l *deferredLogger) enqueuePayload(write func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.append(write)
}

// append queues log and starts the drainer unless it runs. The caller
// holds mu.
func (l *deferredLogger) append(log func()) {
	l.queue = append(l.queue, log)
	if !l.draining {
		l.draining = true
		go l.drain()
	}
}

// drain logs the queued diagnostics until none is left, then the number of
// dropped ones, and ends.
func (l *deferredLogger) drain() {
	for {
		log, ok := l.next()
		if !ok {
			return
		}
		log()
	}
}

// next takes the next diagnostic off the queue; without one it ends the
// draining (false) and wakes flush.
func (l *deferredLogger) next() (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queue) > 0 {
		log := l.queue[0]
		l.queue[0] = nil
		l.queue = l.queue[1:]
		return log, true
	}
	if dropped := l.dropped; dropped > 0 {
		l.dropped = 0
		return func() {
			l.target.Warn("Dropped mapreduce diagnostics while the client log did not take them", dropped)
		}, true
	}
	l.queue = nil
	l.draining = false
	l.drained.Broadcast()
	return nil, false
}

// flush waits until every queued diagnostic was logged. Unlike logging
// through l it blocks while the target does; the client calls it at its
// end, with no lock held, so that its last diagnostics are not lost.
func (l *deferredLogger) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for l.draining {
		l.drained.Wait()
	}
}

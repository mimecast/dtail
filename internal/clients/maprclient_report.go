package clients

import (
	"time"

	maprclient "github.com/mimecast/dtail/internal/mapr/client"
)

// firstReportSettle is how long the reporter waits after the first
// aggregates of every live server of a query generation arrived before it
// reports them (a server's first result may take several writes). Until
// then it waits for the servers still missing, at most half the interval
// after the first server's (firstReportWindow).
const firstReportSettle = time.Second

// earlyFirstReport reports whether the first result of a query generation is
// reported right after its first aggregates arrived (see reportGeneration):
// in every mode with interim reports but NonCumulativeMode, the continuous
// jobs, which keep their fixed cadence so that each of their outfile
// appends covers one interval.
func (c *MaprClient) earlyFirstReport() bool {
	return c.mode != NonCumulativeMode
}

// firstReportWindow times the first report of a query generation with data:
// once every live server's first aggregates arrived (allLiveArrived),
// firstReportSettle after the last of them; until then the window stays
// open for the missing ones, but no longer than half an interval after the
// first server's, so that it stays clear of the servers' next sends. A
// server trailing by more than that (or one with no rows to send) is in
// the next table. Both bounds are capped at half the interval: at
// "interval 1" (and "interval 0") a report 1 s after a send would fall on
// the instant of the next one and mix two intervals in one table.
type firstReportWindow struct {
	// arrived is the generation's AggregatesArrived until the first report
	// with data ran; nil after it, and when the early report is off.
	arrived <-chan struct{}
	// wait is firstReportSettle capped at half the interval; limit is the
	// length of the whole window, half the interval.
	wait, limit time.Duration
	// deadline is the end of the window, set by the first arrival.
	deadline time.Time
}

// newFirstReportWindow returns the window of snapshot's generation.
func (c *MaprClient) newFirstReportWindow(snapshot maprclient.SessionSnapshot,
	interval time.Duration) *firstReportWindow {

	window := &firstReportWindow{limit: interval / 2}
	window.wait = min(firstReportSettle, window.limit)
	if c.earlyFirstReport() {
		window.arrived = snapshot.AggregatesArrived
	}
	return window
}

// settle returns the delay of the next report after a server's first
// aggregates arrived at now: firstReportSettle (capped) once all live
// servers' arrived, else the rest of the window.
func (w *firstReportWindow) settle(now time.Time, all bool) time.Duration {
	if w.deadline.IsZero() {
		w.deadline = now.Add(w.limit)
	}
	rest := max(0, w.deadline.Sub(now))
	if !all {
		return rest
	}
	return min(w.wait, rest)
}

// allLiveArrived reports whether every live server's first aggregates of
// snapshot's generation arrived. Liveness is sampled when an arrival is
// signaled: a missing server whose connection ends later does not shorten
// the wait, which then ends at the window's cap.
func (c *MaprClient) allLiveArrived(snapshot maprclient.SessionSnapshot) bool {
	for _, server := range c.liveServerNames() {
		if !snapshot.ArrivedFrom(server) {
			return false
		}
	}
	return true
}

// liveServerNames are the servers the first report waits for: those of the
// connections whose handler is not done (up, or still connecting).
func (c *MaprClient) liveServerNames() []string {
	if c.liveServers != nil {
		return c.liveServers()
	}
	_, _, connections := c.snapshotMutableState()
	var live []string
	for _, conn := range connections {
		if !connectionEnded(conn) {
			live = append(live, conn.Server())
		}
	}
	return live
}

// reporting is called before every report. A report after an arrival (the
// settled one) ends the window. So does an arrival signaled but not taken
// yet: its aggregates are already merged and so in this report, and a
// settled report right after it would print a second table of almost
// nothing. Before any arrival the window stays open.
func (w *firstReportWindow) reporting() {
	if w.arrived == nil {
		return
	}
	if !w.deadline.IsZero() {
		w.arrived = nil
		return
	}
	select {
	case <-w.arrived:
		w.arrived = nil
	default:
	}
}

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
//
// While servers are missing, the window is looked at again every wait
// (postpone), so a missing server whose connection ends meanwhile, and
// with it the reason to wait, no longer holds the table until the cap.
type firstReportWindow struct {
	// arrived is the generation's AggregatesArrived until the first report
	// with data ran; nil after it, and when the early report is off.
	arrived <-chan struct{}
	// wait is firstReportSettle capped at half the interval; limit is the
	// length of the whole window, half the interval.
	wait, limit time.Duration
	// deadline is the end of the window, set by the first arrival.
	deadline time.Time
	// missing is set while the window waits for live servers whose
	// aggregates did not arrive at the last look (settle).
	missing bool
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

// settle returns the delay of the next look at the window after a
// server's first aggregates arrived at now: firstReportSettle (capped)
// until the report once all live servers' arrived (all), else until the
// servers are looked at again (postpone). It never passes the window's
// end.
func (w *firstReportWindow) settle(now time.Time, all bool) time.Duration {
	if w.deadline.IsZero() {
		w.deadline = now.Add(w.limit)
	}
	w.missing = !all
	return min(w.wait, max(0, w.deadline.Sub(now)))
}

// postpone reports whether the report that is due at now is put off, and
// for how long. Two cases:
//
// A server's first aggregates were signaled but the timer won the select
// (both were ready): they are taken as the arrival they are, so the
// report follows after settle like after any arrival. Reporting right
// away instead put every later report, one interval apart, on the
// instants the servers send at, where a table can hold two sends of one
// server and none of another.
//
// The window waits for missing servers and is still open: it is put off
// by another wait unless all live servers' aggregates arrived by now,
// i.e. the missing ones' connections ended. all is only called while the
// window is open.
func (w *firstReportWindow) postpone(now time.Time, all func() bool) (time.Duration, bool) {
	if w.arrived == nil {
		return 0, false
	}
	select {
	case <-w.arrived:
		if w.deadline.IsZero() || w.missing {
			return w.settle(now, all()), true
		}
	default:
	}
	if !w.missing || !now.Before(w.deadline) || all() {
		return 0, false
	}
	// A look a few nanoseconds before the window's end puts the report
	// off by just that: the timer never fires early, so the next look is
	// at or past the end and reports. It costs one more pass, no spin.
	return min(w.wait, w.deadline.Sub(now)), true
}

// reportSchedule arms the reporter's timer and remembers the instant it
// is due at, so that the periodic reports are planned from the previous
// planned instant and not from the end of the previous report: a cadence
// of interval plus the time a report took would drift towards the
// servers' sends.
type reportSchedule struct {
	timer *time.Timer
	due   time.Time
}

// newReportSchedule returns a schedule whose timer fires delay after now.
func newReportSchedule(now time.Time, delay time.Duration) *reportSchedule {
	return &reportSchedule{timer: time.NewTimer(delay), due: now.Add(delay)}
}

// in makes the next report due delay after now.
func (s *reportSchedule) in(now time.Time, delay time.Duration) {
	s.due = now.Add(delay)
	s.timer.Reset(delay)
}

// next makes the next report due one interval after the instant the
// report that just ran was due at (nextReportDue).
func (s *reportSchedule) next(now time.Time, interval time.Duration) {
	s.due = nextReportDue(s.due, now, interval)
	s.timer.Reset(s.due.Sub(now))
}

// nextReportDue returns the first instant after now that is a whole
// number of intervals after due, the instant the last report was planned
// for. A report that took longer than an interval skips the instants it
// missed and keeps the phase.
func nextReportDue(due, now time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		return now
	}
	next := due.Add(interval)
	if late := now.Sub(next); late >= 0 {
		next = next.Add((late/interval + 1) * interval)
	}
	return next
}

// allLiveArrived reports whether every live server's first aggregates of
// snapshot's generation arrived. Liveness is sampled when an arrival is
// signaled and, while servers are missing, every firstReportWindow.wait
// (postpone).
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
	_, _, connections := c.snapshotMutableState()
	var live []string
	for _, conn := range connections {
		if !connectionEnded(conn) {
			live = append(live, conn.Server())
		}
	}
	return live
}

// reporting is called before every report that postpone did not put off.
// A report after an arrival (the settled one) ends the window; before any
// arrival the window stays open.
func (w *firstReportWindow) reporting() {
	if !w.deadline.IsZero() {
		w.arrived = nil
	}
}

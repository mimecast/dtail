package clients

import (
	"sync"
	"time"
)

// probeNowWait is how long :probe (probeNow) waits for the probe round it
// requested before it answers that the answers are still to come. The
// control loop reads no line meanwhile; the server stops sampling after
// 10 s, but a probe normally answers within milliseconds.
const probeNowWait = 5 * time.Second

// probeOutcome is what came of one probe of a round.
type probeOutcome int

const (
	// probeFailed: the probe got no answer (an error, e.g. the round's
	// timeout or a connection that ended).
	probeFailed probeOutcome = iota
	// probeAnswered: the answer was stored in the field catalog.
	probeAnswered
	// probeBusy: the server answered that another probe of the session was
	// running; nothing was stored. Only dserver answers busy (e.g. a probe
	// the client gave up waiting for still samples there): the client's own
	// probes of a connection wait for each other (handlers.Probe).
	probeBusy
)

// probeRoundResult is what one probe round did: how many files it asked
// about (none when the session reads no files but stdin) and which servers
// answered, answered busy or failed. No server at all means no connection
// could be probed.
type probeRoundResult struct {
	files    int
	answered []string
	busy     []string
	failed   []string
}

// probeWaitStatus is how probeNow ended.
type probeWaitStatus int

const (
	// probeWaitDone: a round started after the request finished.
	probeWaitDone probeWaitStatus = iota
	// probeWaitTimeout: no such round finished within the wait.
	probeWaitTimeout
	// probeWaitStopped: there is no running prober (none, or it stopped
	// with the session).
	probeWaitStopped
)

// probeRounds counts the started and finished rounds of a prober and keeps
// the result of the last finished one, so that probeNow can wait for a
// round that started after its request. Its zero value is ready to use.
type probeRounds struct {
	mu       sync.Mutex
	started  uint64
	finished uint64
	last     probeRoundResult
	// changed is closed and replaced whenever a round finishes.
	changed chan struct{}
}

// newProbeRoundResult sorts the outcomes of a round's targets by kind.
func newProbeRoundResult(files int, targets []probeTarget, outcomes []probeOutcome) probeRoundResult {
	result := probeRoundResult{files: files}
	for i, target := range targets {
		switch outcomes[i] {
		case probeAnswered:
			result.answered = append(result.answered, target.server)
		case probeBusy:
			result.busy = append(result.busy, target.server)
		default:
			result.failed = append(result.failed, target.server)
		}
	}
	return result
}

// probeNow requests a probe round (:probe) and waits until a round that
// started after the request finished, at most probeNowWait, and returns its
// result.
// The round that runs when probeNow is called asked for the files and
// connections of before; the one after it reads the current ones. The wait
// ends early when the prober stops with the session. With server
// connections, a probe's answer is read by the connection handler that also
// writes the output, so the caller must not hold the output paused while it
// waits (see interactiveProber).
func (p *interactiveProber) probeNow() (probeRoundResult, probeWaitStatus) {
	if p == nil {
		return probeRoundResult{}, probeWaitStopped
	}
	target, changed := p.rounds.next()
	p.refreshListings.Store(true)
	p.request()
	timer := time.NewTimer(probeNowWait)
	defer timer.Stop()
	for {
		select {
		case <-p.done:
			return probeRoundResult{}, probeWaitStopped
		case <-timer.C:
			return probeRoundResult{}, probeWaitTimeout
		case <-changed:
		}
		var result probeRoundResult
		var finished bool
		if result, finished, changed = p.rounds.finishedSince(target); finished {
			return result, probeWaitDone
		}
	}
}

// begin counts a round as started.
func (r *probeRounds) begin() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started++
}

// end counts a round as finished with result and wakes the waiters.
func (r *probeRounds) end(result probeRoundResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished++
	r.last = result
	if r.changed != nil {
		close(r.changed)
	}
	r.changed = make(chan struct{})
}

// next returns the number of the next round to start and the channel closed
// when a round finishes next.
func (r *probeRounds) next() (uint64, <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started + 1, r.changedLocked()
}

// finishedSince returns the last result if round number target finished,
// and otherwise the channel closed when a round finishes next. Rounds run
// one at a time, so the last finished round is target or a later one.
func (r *probeRounds) finishedSince(target uint64) (probeRoundResult, bool, <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished >= target {
		return r.last, true, nil
	}
	return probeRoundResult{}, false, r.changedLocked()
}

func (r *probeRounds) changedLocked() chan struct{} {
	if r.changed == nil {
		r.changed = make(chan struct{})
	}
	return r.changed
}

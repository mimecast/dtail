package readhub

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
)

// An evicted session reads privately only until it caught up: whenever its
// private reader reaches the end of the file at the start of a line, at
// offset P of file F, the session rejoins the shared reader of its file if
// that reader has F open, is not in the middle of a read, and has neither
// read past P nor published a line ending past P yet (see entry.rejoin), or
// starts a new shared reader at P when there is none. The private reader then
// stops without reading past P, and the session skips the
// published lines that end at or before P, so it gets every line of the file
// once, with the same filter and processor, like one private read. Otherwise
// it goes on privately and tries again at a later end of the file, after a
// pause that grows with every declined attempt and every eviction soon after
// a rejoin, so that a session too slow for the shared reader does not
// alternate between it and a private reader all the time.

const (
	// rejoinMinDelay is the pause after the first declined rejoin, and after
	// the first eviction soon after a rejoin.
	rejoinMinDelay = time.Second
	// rejoinMaxDelay bounds the pause; a rejoined session that stayed
	// subscribed at least as long is not considered slow when evicted again.
	rejoinMaxDelay = 30 * time.Second
)

// errRejoined reports that the session's private reader handed the read back
// to a shared reader.
var errRejoined = errors.New("rejoined the shared follow read")

// rejoinPolicy spaces a session's attempts to rejoin a shared reader. It is
// used by the session's goroutine only.
type rejoinPolicy struct {
	now      func() time.Time
	minDelay time.Duration
	maxDelay time.Duration
	// delay is the current pause; zero before the first one.
	delay time.Duration
	// next is when the next attempt is due; zero for at once.
	next time.Time
	// rejoinedAt is when the session rejoined last; zero if it never did.
	rejoinedAt time.Time
}

func newRejoinPolicy(seams hubSeams) rejoinPolicy {
	policy := rejoinPolicy{now: seams.now, minDelay: seams.rejoinMinDelay, maxDelay: seams.rejoinMaxDelay}
	if policy.now == nil {
		policy.now = time.Now
	}
	if policy.minDelay <= 0 {
		policy.minDelay = rejoinMinDelay
	}
	if policy.maxDelay < policy.minDelay {
		policy.maxDelay = max(rejoinMaxDelay, policy.minDelay)
	}
	return policy
}

// due reports whether the session may try to rejoin now.
func (p *rejoinPolicy) due() bool {
	return p.next.IsZero() || !p.now().Before(p.next)
}

// declined pauses the attempts after one was declined.
func (p *rejoinPolicy) declined() {
	p.backOff(p.now())
}

// rejoined records a successful attempt.
func (p *rejoinPolicy) rejoined() {
	p.rejoinedAt = p.now()
}

// evicted pauses the attempts when the session is evicted again soon after it
// rejoined; after a longer stay, or the first eviction, it may rejoin at once.
func (p *rejoinPolicy) evicted() {
	now := p.now()
	if !p.rejoinedAt.IsZero() && now.Sub(p.rejoinedAt) < p.maxDelay {
		p.backOff(now)
		return
	}
	p.delay = 0
	p.next = time.Time{}
}

func (p *rejoinPolicy) backOff(now time.Time) {
	p.delay = min(max(2*p.delay, p.minDelay), p.maxDelay)
	p.next = now.Add(p.delay)
}

// rejoin moves sub, which reads privately and got to at, the end of the file
// at the start of a line, back to a shared reader of its file: the running
// one, if it can take the session there (see entry.rejoin), or a new one
// that starts at at, if there is none. It reports whether sub rejoined; its private reader must
// then stop, as the shared reader feeds it every line past at. It runs on the
// session's goroutine.
func (h *Hub) rejoin(sub *subscriber, at position) bool {
	key := sessionKey(sub.session)
	h.mu.Lock()
	e := h.entries[key]
	if e == nil || e.isClosed() {
		defer h.mu.Unlock()
		return h.startRejoined(key, sub, at)
	}
	// As for a join, a publication in progress may take the hub's lock.
	h.mu.Unlock()

	// A descriptor of at's file for the session to hold (see heldFile),
	// through its own target: the path may be rotated any moment. Without
	// it, the session stays private for now.
	held := openSame(h.seams.openFile, sub.session.Target, at.file)
	if held == nil {
		return false
	}
	if !e.rejoin(sub, at, held) {
		closeHeld(held)
		return false
	}
	return true
}

// startRejoined starts a new shared read of sub's file at at, where sub's
// private read got to, with sub as its only subscriber. Its reader starts in
// a descriptor of at's file opened through sub's target, so that a rotation
// of the path since costs no line; if the path was rotated already, sub stays
// private for now. The caller holds h.mu.
func (h *Hub) startRejoined(key entryKey, sub *subscriber, at position) bool {
	start := openSame(h.seams.openFile, sub.session.Target, at.file)
	if start == nil {
		return false
	}
	e := newEntry(key, sub.session, at, start, h.options, h.logger, h.seams, h.forget)
	h.entries[key] = e
	// A new entry is open. The session holds a descriptor of the file once
	// the reader opened it, as a joining session of a new entry does.
	_, _ = e.add(sub)
	sub.resubscribe(e, nil)
	e.start()
	e.logRejoin(1)
	return true
}

// openTarget opens target's file.
func openTarget(target fs.ValidatedReadTarget) (*os.File, error) {
	return target.Open()
}

// rejoin adds sub, an evicted session whose private read got to at, back to
// the running entry, holding fd, a descriptor of at's file, if the entry
// feeds it every line of that file ending past at: the reader has that file
// open, is not in a read, has not read past at and has not published such a
// line yet (see feedsPast). sub then skips the published lines ending at or
// before at. Nothing is published meanwhile, so no line is lost and none
// delivered twice.
func (e *entry) rejoin(sub *subscriber, at position, fd *os.File) bool {
	e.publishMu.Lock()
	defer e.publishMu.Unlock()
	if !e.feedsPast(at) {
		return false
	}
	count, added := e.add(sub)
	if !added {
		return false
	}
	sub.resubscribe(e, fd)
	e.logRejoin(count)
	return true
}

// feedsPast reports whether the reader is going to publish every line of at's
// file that ends past at, and none of them was published yet, and it has not
// read past at either. The caller holds e.publishMu.
//
// The reader must not have read past at, not only not published past it: it
// may have read bytes past at it has not published, e.g. an unfinished line.
// Should the file then be truncated to a size between at and them, the reader
// rewinds and publishes a restart, and the session, whose private read took
// the rewritten bytes up to at as the continuation of the file, would get
// them again. readPos tells how far the reader read only between two reads:
// a read returns its bytes before readUpTo records where it got to, so
// feedsPast declines while e.reading says a read is under way. A read that
// starts after the check is harmless: the flag is atomic, so a reader that
// sets it after the load here reads after it too, and it publishes that
// read's lines under publishMu, so only once the session is subscribed. A
// declined session tries again at a later end of the file; the reader is in
// a read only for the moment a read takes.
func (e *entry) feedsPast(at position) bool {
	switch {
	case at.file == nil || at.offset < 0:
		return false
	case !e.published.known() || e.published.file == nil || !os.SameFile(e.published.file, at.file):
		// Between a reopen and the reader opening the file, or reading
		// another file: the published position is that of the file the
		// reader has open.
		return false
	case e.reading.Load():
		// In a read: it may have read past at already.
		return false
	case e.readPos.file != nil && os.SameFile(e.readPos.file, at.file) && e.readPos.offset > at.offset:
		// It read bytes past at it has not published yet.
		return false
	}
	return e.published.offset <= at.offset
}

func (e *entry) logRejoin(count int) {
	e.logger.Info(e.path, "Evicted subscriber rejoined the shared follow read", fmt.Sprintf("subscribers=%d", count))
}

package handlers

import (
	"sync"
	"time"
)

// busyClock measures how long a handler was busy handling received frames
// (its Write method). While it is, the handler reads no further frame: a
// payload write blocks while the interactive output is paused (or a host
// key prompt holds the terminal, or a pipe reader stalls), and the frames
// behind it, a session acknowledgement among them, wait. WaitForSessionAck
// uses it so that this time does not count against its timeout.
//
// A Write takes the lock twice, once per call and not per frame; the calls
// come from the single goroutine copying the server's output, so the lock
// is contended only by a waiter reading the clock.
type busyClock struct {
	mu sync.Mutex
	// since is when the running busy period began; zero while idle.
	since time.Time
	// total is the length of all finished busy periods.
	total time.Duration
}

// begin starts a busy period.
func (c *busyClock) begin() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.since = time.Now()
}

// end ends the busy period begin started.
func (c *busyClock) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += time.Since(c.since)
	c.since = time.Time{}
}

// busy returns how long the handler was busy up to now, the running busy
// period included. Only differences of two results mean anything.
func (c *busyClock) busy(now time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.since.IsZero() {
		return c.total
	}
	return c.total + now.Sub(c.since)
}

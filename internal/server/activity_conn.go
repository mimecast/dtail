package server

import (
	"net"
	"sync"
	"time"
)

// activityConn applies a rolling deadline after the SSH handshake. Successful
// reads and writes refresh the deadline, so active streams remain connected.
type activityConn struct {
	net.Conn

	mu      sync.Mutex
	timeout time.Duration
	now     func() time.Time
}

func newActivityConn(conn net.Conn) *activityConn {
	return &activityConn{Conn: conn, now: time.Now}
}

func (c *activityConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		if deadlineErr := c.refreshDeadline(); err == nil && deadlineErr != nil {
			err = deadlineErr
		}
	}
	return n, err
}

func (c *activityConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		if deadlineErr := c.refreshDeadline(); err == nil && deadlineErr != nil {
			err = deadlineErr
		}
	}
	return n, err
}

func (c *activityConn) enable(timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.timeout = timeout
	return c.SetDeadline(c.now().Add(timeout))
}

func (c *activityConn) refreshDeadline() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.timeout <= 0 {
		return nil
	}
	return c.SetDeadline(c.now().Add(c.timeout))
}

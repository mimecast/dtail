package connectors

import (
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// handshakeDeadline bounds the SSH handshake of a client connection (version
// exchange, key exchange and authentication) with a deadline on its TCP
// connection, so a host that accepts the TCP connection but never completes
// the handshake fails the connection instead of leaving it connecting
// forever. ssh.NewClientConn has no timeout of its own, and the connect
// timeout (SSHConnectTimeoutMs) bounds only the TCP dial.
//
// The time spent in the host key callback does not count: the known_hosts
// check may ask the user whether to trust an unknown host, and the answer
// may take longer than the whole timeout. The callback pauses the deadline
// (clearing it also releases the read the SSH transport keeps pending) and
// resumes it with the time that was left. Once the handshake ended, finish
// clears the deadline for good; the callback, which x/crypto/ssh also calls
// for every later re-key, then leaves the connection alone.
type handshakeDeadline struct {
	conn net.Conn
	mu   sync.Mutex
	// deadline is the armed deadline, zero while paused or finished.
	deadline time.Time
	// remaining is the time left when the deadline was paused.
	remaining time.Duration
	paused    bool
	finished  bool
	// expired records that the deadline had passed when the handshake ended.
	expired bool
}

// startHandshakeDeadline arms a deadline of timeout on conn.
func startHandshakeDeadline(conn net.Conn, timeout time.Duration) (*handshakeDeadline, error) {
	d := &handshakeDeadline{conn: conn}
	if err := d.arm(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("set SSH handshake deadline: %w", err)
	}
	return d, nil
}

// arm sets the deadline on the connection; the caller holds mu or owns d.
func (d *handshakeDeadline) arm(deadline time.Time) error {
	d.deadline = deadline
	return d.conn.SetDeadline(deadline)
}

// wrapHostKeyCallback returns callback with the deadline paused while it runs.
func (d *handshakeDeadline) wrapHostKeyCallback(callback ssh.HostKeyCallback) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		d.pause()
		defer d.resume()
		return callback(hostname, remote, key)
	}
}

func (d *handshakeDeadline) pause() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finished || d.paused {
		return
	}
	d.paused = true
	d.remaining = time.Until(d.deadline)
	d.deadline = time.Time{}
	// An error here leaves the deadline running, which can only end the
	// handshake early; the connection is then broken anyway.
	_ = d.conn.SetDeadline(time.Time{})
}

func (d *handshakeDeadline) resume() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finished || !d.paused {
		return
	}
	d.paused = false
	// A failed SetDeadline leaves the connection without a deadline; the
	// handshake then fails on the broken connection by itself.
	_ = d.arm(time.Now().Add(d.remaining))
}

// finish clears the deadline once the handshake ended and reports whether
// it had expired, i.e. whether the handshake failed because it timed out.
func (d *handshakeDeadline) finish() (expired bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.finished {
		d.finished = true
		d.expired = !d.deadline.IsZero() && !time.Now().Before(d.deadline)
		d.deadline = time.Time{}
		err = d.conn.SetDeadline(time.Time{})
	}
	return d.expired, err
}

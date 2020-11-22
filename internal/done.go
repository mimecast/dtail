package internal

import (
	"sync"
)

// Done is a cleanup/shutdown helper.
type Done struct {
	ch    chan struct{}
	mutex sync.Mutex
}

// NewDone returns a new cleanup/shutdown helper.
func NewDone() *Done {
	return &Done{
		ch: make(chan struct{}),
	}
}

// Done returns the done channel (closed when done)
func (d *Done) Done() <-chan struct{} {
	return d.ch
}

// Shutdown closes the done channel. It can be called multiple times.
func (d *Done) Shutdown() {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	select {
	case <-d.ch:
		return
	default:
		close(d.ch)
	}
}

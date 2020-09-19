package internal

import (
	"sync"
)

type Done struct {
	ch    chan struct{}
	mutex sync.Mutex
}

func NewDone() *Done {
	return &Done{
		ch: make(chan struct{}),
	}
}

func (d *Done) Done() <-chan struct{} {
	return d.ch
}

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

package handlers

import (
	"sync"

	"github.com/mimecast/dtail/internal/io/logger"
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

	logger.Debug("Done.Shutdown()")

	select {
	case <-d.ch:
		return
	default:
		logger.Debug("Done.Shutdown() -> close")
		close(d.ch)
	}
}

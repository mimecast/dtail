package background

import (
	"context"
	"fmt"
	"sync"

	"github.com/mimecast/dtail/internal/io/logger"
)

type job struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

type Background struct {
	mutex sync.Mutex
	jobs  map[string]job
}

func NewBackground() *Background {
	return &Background{
		jobs: make(map[string]job),
	}
}

func (b Background) Add(name string, cancel context.CancelFunc, done <-chan struct{}) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if _, ok := b.jobs[name]; ok {
		return fmt.Errorf("job '%s' already exists", name)
	}

	logger.Debug("background", name, "adding job")
	b.jobs[name] = job{cancel, done}

	return nil
}

func (b Background) get(name string) (job, bool) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	job, ok := b.jobs[name]
	return job, ok
}

func (b Background) delete(name string) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	delete(b.jobs, name)
}

func (b Background) Stop(name string) {
	logger.Debug("background", name, "stopping job")
	job, ok := b.get(name)

	if !ok {
		logger.Debug("background", name, "no such job")
		return
	}

	logger.Debug("background", name, "canceling job")
	job.cancel()

	logger.Debug("background", name, "waiting for job to complete")
	<-job.done

	logger.Debug("background", name, "deleting job")
	b.delete(name)
}

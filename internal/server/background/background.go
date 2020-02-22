package background

import (
	"context"
	"errors"
	"sync"
)

type job struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

// Background specifies a job or command run in background on server side.
// This does not require an active DTail client SSH connection/session.
type Background struct {
	mutex *sync.Mutex
	jobs  map[string]job
}

// New returns a new background manager.
func New() Background {
	return Background{
		jobs:  make(map[string]job),
		mutex: &sync.Mutex{},
	}
}

// Add a background job.
func (b Background) Add(name string, cancel context.CancelFunc, done <-chan struct{}) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if _, ok := b.jobs[name]; ok {
		return errors.New("job already exists")
	}

	b.jobs[name] = job{cancel, done}
	return nil
}

// Cancel a background job.
func (b Background) Cancel(name string) error {
	job, ok := b.get(name)
	if !ok {
		return errors.New("no job to cancel")
	}

	job.cancel()
	<-job.done
	b.delete(name)

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

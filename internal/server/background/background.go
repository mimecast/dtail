package background

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/mimecast/dtail/internal/io/logger"
)

type job struct {
	cancel context.CancelFunc
	wg     *sync.WaitGroup
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
func (b Background) Add(userName, jobName string, cancel context.CancelFunc, wg *sync.WaitGroup) error {
	key := b.key(userName, jobName)

	b.mutex.Lock()
	defer b.mutex.Unlock()

	if _, ok := b.jobs[key]; ok {
		return errors.New("job already exists")
	}

	b.jobs[key] = job{cancel, wg}

	// Clean up background job database.
	go func() {
		wg.Wait()
		b.cancel(key)
	}()

	return nil
}

// Cancel a background job.
func (b Background) Cancel(userName, jobName string) error {
	return b.cancel(b.key(userName, jobName))
}

func (b Background) cancel(key string) error {
	job, ok := b.get(key)
	if !ok {
		return errors.New("no job to cancel")
	}

	job.cancel()
	job.wg.Wait()
	b.delete(key)

	return nil
}

// ListJobsC returns a channel listing all jobs of the given user.
func (b Background) ListJobsC(userName string) <-chan string {
	ch := make(chan string)

	go func() {
		defer close(ch)

		b.mutex.Lock()
		defer b.mutex.Unlock()

		for k, _ := range b.jobs {
			logger.Debug("ListJobsC", k, userName)
			if strings.HasPrefix(k, fmt.Sprintf("%s.", userName)) {
				ch <- k
			}
		}
	}()

	return ch
}

func (b Background) get(key string) (job, bool) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	job, ok := b.jobs[key]
	return job, ok
}

func (b Background) delete(key string) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	delete(b.jobs, key)
}

func (Background) key(userName, jobName string) string {
	return fmt.Sprintf("%s.%s", userName, jobName)
}

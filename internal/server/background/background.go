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
	logger.Debug("background", "Add", key)

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
	key := b.key(userName, jobName)
	logger.Debug("background", "Cancel", key)

	return b.cancel(key)
}

func (b Background) cancel(key string) error {
	job, ok := b.get(key)
	logger.Debug("background", "cancel", key, job, ok)

	if !ok {
		return errors.New("no job to cancel")
	}

	logger.Debug("background", "cancel", "run job.cancel()")
	job.cancel()
	logger.Debug("background", "cancel", "run job.wg.Wait()")
	job.wg.Wait()
	logger.Debug("background", "cancel", "run b.delete(key)")
	b.delete(key)

	return nil
}

// ListJobsC returns a channel listing all jobs of the given user.
func (b Background) ListJobsC(userName string) <-chan string {
	logger.Debug("background", "ListJobC", userName)

	ch := make(chan string)

	go func() {
		defer close(ch)

		b.mutex.Lock()
		defer b.mutex.Unlock()

		for k := range b.jobs {
			logger.Debug("ListJobsC", k, userName)
			if strings.HasPrefix(k, fmt.Sprintf("%s.", userName)) {
				ch <- k
			}
		}
	}()

	return ch
}

func (b Background) get(key string) (job, bool) {
	logger.Debug("background", "get", key)

	b.mutex.Lock()
	defer b.mutex.Unlock()

	job, ok := b.jobs[key]
	return job, ok
}

func (b Background) delete(key string) {
	logger.Debug("background", "delete", key)

	b.mutex.Lock()
	defer b.mutex.Unlock()

	delete(b.jobs, key)
}

func (Background) key(userName, jobName string) string {
	return fmt.Sprintf("%s.%s", userName, jobName)
}

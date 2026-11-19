package connectors

import (
	"sync"
	"time"
)

// applyLock is a mutex that can also be waited for with a time limit
// (lockWithin). It serializes the session transitions of one connection
// (committedSessionState.applyMu). The initial SESSION START holds it for
// its whole acknowledgement wait, which has no wall-clock limit (a paused
// output only delays it, see handlers.Handler.WaitForSessionAck), so the
// interactive control loop, which must not wait without bound, takes it
// with lockWithin. The zero value is unlocked.
type applyLock struct {
	once sync.Once
	ch   chan struct{}
}

func (l *applyLock) sem() chan struct{} {
	l.once.Do(func() { l.ch = make(chan struct{}, 1) })
	return l.ch
}

// Lock waits until the lock is free and takes it.
func (l *applyLock) Lock() {
	l.sem() <- struct{}{}
}

// lockWithin takes the lock if it becomes free within d, and reports
// whether it did.
func (l *applyLock) lockWithin(d time.Duration) bool {
	sem := l.sem()
	select {
	case sem <- struct{}{}:
		return true
	default:
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case sem <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

// Unlock releases the lock. Unlocking a lock that is not locked is a bug
// of the caller and panics, as it does for sync.Mutex, instead of blocking
// forever on the empty semaphore.
func (l *applyLock) Unlock() {
	select {
	case <-l.sem():
	default:
		panic("connectors: unlock of unlocked applyLock")
	}
}

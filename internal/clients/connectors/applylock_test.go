package connectors

import (
	"testing"
	"time"
)

// TestApplyLockUnlockOfUnlockedPanics: unlocking an applyLock that is not
// locked panics, as for sync.Mutex, instead of blocking forever.
func TestApplyLockUnlockOfUnlockedPanics(t *testing.T) {
	for name, prepare := range map[string]func(*applyLock){
		"zero value": func(*applyLock) {},
		"unlocked before": func(l *applyLock) {
			if l.lockWithin(time.Second) {
				l.Unlock()
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var lock applyLock
			prepare(&lock)
			done := make(chan any, 1)
			go func() {
				defer func() { done <- recover() }()
				lock.Unlock()
			}()
			select {
			case recovered := <-done:
				if recovered == nil {
					t.Fatal("Unlock of an unlocked applyLock did not panic")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Unlock of an unlocked applyLock blocked")
			}
		})
	}
}

// TestApplyLockLockWithin: lockWithin takes a free lock at once and gives
// up on a held one after its wait, and Unlock frees it again.
func TestApplyLockLockWithin(t *testing.T) {
	var lock applyLock
	if !lock.lockWithin(time.Millisecond) {
		t.Fatal("lockWithin() did not take a free lock")
	}
	if lock.lockWithin(20 * time.Millisecond) {
		t.Fatal("lockWithin() took a held lock")
	}
	lock.Unlock()
	if !lock.lockWithin(time.Millisecond) {
		t.Fatal("lockWithin() did not take the lock after Unlock")
	}
	lock.Unlock()
}

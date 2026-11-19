package clients

import (
	"testing"
	"time"
)

// TestMaprClientFinishAwaitsReload: the final report waits for a reload in
// flight, whose end merges the aggregates held during it, and does not
// wait when none is.
func TestMaprClientFinishAwaitsReload(t *testing.T) {
	c := &MaprClient{baseClient: baseClient{mu: newBaseClientMu()}}
	c.awaitReload()

	reloadMu := c.reloadLock()
	reloadMu.Lock()
	awaited := make(chan struct{})
	go func() {
		c.awaitReload()
		close(awaited)
	}()
	select {
	case <-awaited:
		t.Fatal("awaitReload returned while a reload held the reload lock")
	case <-time.After(50 * time.Millisecond):
	}
	reloadMu.Unlock()
	<-awaited
}

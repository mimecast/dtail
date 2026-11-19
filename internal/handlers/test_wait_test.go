package handlers

import (
	"testing"
	"time"
)

func waitForHandlerCondition(t *testing.T, timeout time.Duration, failure string,
	condition func() bool, diagnostic ...func() string) {

	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			if len(diagnostic) > 0 {
				t.Fatalf("%s: %s", failure, diagnostic[0]())
			}
			t.Fatal(failure)
		}
	}
}

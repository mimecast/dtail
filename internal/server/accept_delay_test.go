package server

import (
	"testing"
	"time"
)

func TestNextAcceptDelay(t *testing.T) {
	delay := nextAcceptDelay(0)
	if delay != 5*time.Millisecond {
		t.Fatalf("first delay = %v, want 5ms", delay)
	}
	for i := 0; i < 20; i++ {
		next := nextAcceptDelay(delay)
		if next < delay || next > time.Second {
			t.Fatalf("delay after %v = %v", delay, next)
		}
		delay = next
	}
	if delay != time.Second {
		t.Fatalf("delay did not reach its maximum: %v", delay)
	}
}

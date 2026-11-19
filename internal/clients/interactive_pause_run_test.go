package clients

import "testing"

// TestRunOutputKeepsPauses: runOutput releases only the control loop's own
// pause for the time of fn and restores it after, whether it is an explicit
// pause or one for typing; a pause held elsewhere (the host key prompt or
// the interrupt statistics, which pause the logger too) stays all along,
// and output that is not paused is left alone.
func TestRunOutputKeepsPauses(t *testing.T) {
	for _, tt := range []struct {
		name   string
		typing bool
		extra  int
	}{
		{name: "explicit"},
		{name: "typing", typing: true},
		{name: "explicit with another pause", extra: 1},
		{name: "typing with two other pauses", typing: true, extra: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logger := newPausingTestLogger()
			client := newPauseTestClient(logger)
			pause := client.newInteractivePause()
			for range tt.extra {
				logger.Pause()
			}
			if tt.typing {
				pause.pauseTyping()
			} else {
				pause.pause()
			}
			gate := client.runtime.outputGate
			var during int
			var gatePaused bool
			pause.runOutput(func() {
				during = logger.pauseDepth()
				gate.mu.Lock()
				gatePaused = gate.paused
				gate.mu.Unlock()
			})
			if during != tt.extra || gatePaused {
				t.Fatalf("during fn: depth %d gate paused %v, want depth %d and the gate open",
					during, gatePaused, tt.extra)
			}
			if logger.pauseDepth() != tt.extra+1 || !pause.paused || pause.typing != tt.typing || !gate.paused {
				t.Fatalf("after: depth %d paused %v typing %v gate %v", logger.pauseDepth(), pause.paused,
					pause.typing, gate.paused)
			}
		})
	}

	logger := newPausingTestLogger()
	client := newPauseTestClient(logger)
	pause := client.newInteractivePause()
	ran := false
	pause.runOutput(func() { ran = true })
	if !ran || pause.paused || logger.pauseDepth() != 0 || client.runtime.outputGate.paused {
		t.Fatalf("not paused: ran %v paused %v depth %d", ran, pause.paused, logger.pauseDepth())
	}
}

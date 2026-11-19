package handlers

import (
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

func TestNewReadTimingsSnapshotsServerConfig(t *testing.T) {
	serverCfg := &config.ServerConfig{
		ReadGlobRetryIntervalMs: 11,
		ReadRetryIntervalMs:     12,
		OutputEOFAckTimeoutMs:   13,
		MaxLineLength:           14,
		MaxGlobTargets:          15,
	}

	timings := newReadTimings(serverCfg)
	serverCfg.ReadGlobRetryIntervalMs = 101
	serverCfg.ReadRetryIntervalMs = 102
	serverCfg.OutputEOFAckTimeoutMs = 103
	serverCfg.MaxLineLength = 104
	serverCfg.MaxGlobTargets = 105

	if timings.globRetryInterval != 11*time.Millisecond {
		t.Fatalf("glob retry interval = %s, want 11ms", timings.globRetryInterval)
	}
	if timings.readRetryInterval != 12*time.Millisecond {
		t.Fatalf("read retry interval = %s, want 12ms", timings.readRetryInterval)
	}
	if timings.outputEOFAckTimeout != 13*time.Millisecond {
		t.Fatalf("EOF acknowledgement timeout = %s, want 13ms", timings.outputEOFAckTimeout)
	}
	if timings.maxLineLength != 14 {
		t.Fatalf("maximum line length = %d, want 14", timings.maxLineLength)
	}
	if timings.maxGlobTargets != 15 {
		t.Fatalf("maximum glob targets = %d, want 15", timings.maxGlobTargets)
	}
}

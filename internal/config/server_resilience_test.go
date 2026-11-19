package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDefaultServerResilienceLimits(t *testing.T) {
	cfg := newDefaultServerConfig()
	if cfg.IdleSessionTimeoutS != DefaultIdleSessionTimeoutS || cfg.IdleSessionTimeoutS <= 0 {
		t.Fatalf("IdleSessionTimeoutS = %d, want positive default %d",
			cfg.IdleSessionTimeoutS, DefaultIdleSessionTimeoutS)
	}
	if cfg.OutputBufferMaxBytes != DefaultOutputBufferMaxBytes || cfg.OutputBufferMaxBytes <= 0 {
		t.Fatalf("OutputBufferMaxBytes = %d, want positive default %d",
			cfg.OutputBufferMaxBytes, DefaultOutputBufferMaxBytes)
	}
	if cfg.OutputFlushTimeoutMs <= 0 || cfg.OutputReadRetryIntervalMs <= 0 || cfg.OutputEOFAckTimeoutMs <= 0 {
		t.Fatalf("output deadlines must have positive defaults: flush=%d generationRetry=%d eofAck=%d",
			cfg.OutputFlushTimeoutMs, cfg.OutputReadRetryIntervalMs, cfg.OutputEOFAckTimeoutMs)
	}
}

func TestDefaultServerConfigOmitsRetiredOutputTimingKeys(t *testing.T) {
	encoded, err := json.Marshal(newDefaultServerConfig())
	if err != nil {
		t.Fatalf("marshal default server config: %v", err)
	}
	for _, key := range []string{
		"OutputTransmissionDelayMs",
		"OutputEOFWaitBaseMs",
		"OutputEOFWaitPerFileMs",
		"OutputEOFWaitMaxMs",
		"OutputFlushPollIntervalMs",
		"ShutdownOutputSerializeWaitMs",
		"ShutdownIdleRecheckWaitMs",
	} {
		if strings.Contains(string(encoded), key) {
			t.Errorf("default server config still serializes retired key %q", key)
		}
	}
}

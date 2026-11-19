package dlog

import (
	"strings"
	"testing"
)

func TestNewLevelReturnsErrorForUnknownLevel(t *testing.T) {
	_, err := newLevel("chatty")
	if err == nil || !strings.Contains(err.Error(), "chatty") {
		t.Fatalf("newLevel error = %v, want unknown level", err)
	}
}

// TestTraceEnabled verifies that TraceEnabled mirrors Trace's internal
// maxLevel < Trace early-return: it must report true exactly when the
// configured level is Trace or higher (Devel/All disable trace? no — the level
// ladder is monotonically increasing, so any level >= Trace enables trace),
// and false for every level below Trace.
func TestTraceEnabled(t *testing.T) {
	tests := []struct {
		name  string
		level level
		want  bool
	}{
		{"none", None, false},
		{"fatal", Fatal, false},
		{"error", Error, false},
		{"warn", Warn, false},
		{"info", Info, false},
		{"default", Default, false},
		{"verbose", Verbose, false},
		{"debug", Debug, false},
		{"devel", Devel, false},
		{"trace", Trace, true},
		{"all", All, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &DLog{maxLevel: tc.level}
			if got := d.TraceEnabled(); got != tc.want {
				t.Fatalf("TraceEnabled() at level %v = %v, want %v",
					tc.level, got, tc.want)
			}
			// TraceEnabled must agree with what Trace itself would do: Trace
			// returns "" (no work) exactly when trace is disabled.
			traceDidWork := d.maxLevel >= Trace
			if traceDidWork != tc.want {
				t.Fatalf("TraceEnabled() disagrees with Trace's own gate at level %v", tc.level)
			}
		})
	}
}

// TestTraceEnabledNilSafe guards the footgun that call sites rely on:
// TraceEnabled is invoked on the package-level loggers (Server/Client/Common),
// which are nil until Start runs. A nil receiver must report false, not panic.
func TestTraceEnabledNilSafe(t *testing.T) {
	var d *DLog
	if d.TraceEnabled() {
		t.Fatal("nil *DLog.TraceEnabled() must be false")
	}
}

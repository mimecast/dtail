package logging

import "testing"

type pointerLogger struct{}

func (*pointerLogger) Error(...any) string { return "" }
func (*pointerLogger) Warn(...any) string  { return "" }
func (*pointerLogger) Info(...any) string  { return "" }
func (*pointerLogger) Debug(...any) string { return "" }
func (*pointerLogger) Trace(...any) string { return "" }

func (*pointerLogger) TraceEnabled() bool { return false }

func TestOrNopReplacesTypedNilLogger(t *testing.T) {
	var logger *pointerLogger

	if _, ok := OrNop(logger).(NopLogger); !ok {
		t.Fatal("OrNop did not replace a typed nil logger")
	}
}

func TestOrNopReplacesNilLogger(t *testing.T) {
	if _, ok := OrNop(nil).(NopLogger); !ok {
		t.Fatal("OrNop did not replace a nil logger")
	}
}

func TestOrNopPreservesLogger(t *testing.T) {
	logger := &pointerLogger{}

	if got := OrNop(logger); got != logger {
		t.Fatalf("OrNop returned %T, want the supplied logger", got)
	}
}

// debugLogger is a logger that can tell whether it writes debug messages.
type debugLogger struct {
	NopLogger
	debug bool
}

func (l debugLogger) DebugEnabled() bool { return l.debug }

// TestDebugEnabled checks the optional capability: a logger implementing
// DebugEnabled is asked, and one that cannot tell is assumed to write debug
// messages, so its callers never lose a Debug call.
func TestDebugEnabled(t *testing.T) {
	tests := []struct {
		name   string
		logger Logger
		want   bool
	}{
		{name: "NopLogger", logger: NopLogger{}, want: false},
		{name: "enabler reports enabled", logger: debugLogger{debug: true}, want: true},
		{name: "enabler reports disabled", logger: debugLogger{debug: false}, want: false},
		{name: "logger without the method", logger: &pointerLogger{}, want: true},
		{name: "OrNop of nil", logger: OrNop(nil), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DebugEnabled(tc.logger); got != tc.want {
				t.Fatalf("DebugEnabled(%T) = %v, want %v", tc.logger, got, tc.want)
			}
		})
	}
}

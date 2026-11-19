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

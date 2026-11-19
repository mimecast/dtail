package logging

import (
	"errors"
	"testing"
)

// filteringLogger is a Logger whose level filters every message out, as a
// dserver logger at --logLevel none does; it lacks the MessageLogger
// capability.
type filteringLogger struct{ NopLogger }

// echoLogger is a Logger that returns a logged line, as a logger with the
// level enabled does; it lacks the MessageLogger capability.
type echoLogger struct{ NopLogger }

func (echoLogger) Error(...any) string { return "logged error" }
func (echoLogger) Warn(...any) string  { return "logged warning" }

// messageRecorder implements MessageLogger.
type messageRecorder struct {
	NopLogger
	calls []string
}

func (r *messageRecorder) ErrorMessage(...any) string {
	r.calls = append(r.calls, "ErrorMessage")
	return "error message"
}

func (r *messageRecorder) WarnMessage(...any) string {
	r.calls = append(r.calls, "WarnMessage")
	return "warn message"
}

func TestClientMessagesAreNeverEmpty(t *testing.T) {
	recorder := &messageRecorder{}
	var nilRecorder *messageRecorder
	tests := []struct {
		name    string
		logger  Logger
		message func(Logger, ...any) string
		want    string
	}{
		{name: "warn, filtered", logger: filteringLogger{}, message: WarnMessage,
			want: "WARN|Unable to read file(s)|problem"},
		{name: "error, filtered", logger: filteringLogger{}, message: ErrorMessage,
			want: "ERROR|Unable to read file(s)|problem"},
		{name: "warn, nil logger", logger: nil, message: WarnMessage,
			want: "WARN|Unable to read file(s)|problem"},
		{name: "warn, typed nil logger", logger: nilRecorder, message: WarnMessage,
			want: "WARN|Unable to read file(s)|problem"},
		{name: "warn, logged", logger: echoLogger{}, message: WarnMessage, want: "logged warning"},
		{name: "error, logged", logger: echoLogger{}, message: ErrorMessage, want: "logged error"},
		{name: "warn, capability", logger: recorder, message: WarnMessage, want: "warn message"},
		{name: "error, capability", logger: recorder, message: ErrorMessage, want: "error message"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.message(tt.logger, "Unable to read file(s)", errors.New("problem")); got != tt.want {
				t.Fatalf("message = %q, want %q", got, tt.want)
			}
		})
	}
	if len(recorder.calls) != 2 || recorder.calls[0] != "WarnMessage" || recorder.calls[1] != "ErrorMessage" {
		t.Fatalf("MessageLogger calls = %q, want WarnMessage then ErrorMessage", recorder.calls)
	}
}

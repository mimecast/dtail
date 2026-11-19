package dlog

import (
	"errors"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/source"
)

// TestDLogMessagesIgnoreTheLogLevel checks that ErrorMessage and WarnMessage
// return the client-facing text whatever the logger's level, and write it to
// the sink only when that level is enabled, as Error and Warn do.
func TestDLogMessagesIgnoreTheLogLevel(t *testing.T) {
	tests := []struct {
		name     string
		maxLevel level
		message  func(*DLog, ...any) string
		want     string
		wantLogs int
	}{
		{name: "warn at warn level", maxLevel: Warn, message: (*DLog).WarnMessage,
			want: "WARN|", wantLogs: 1},
		{name: "warn at error level", maxLevel: Error, message: (*DLog).WarnMessage,
			want: "WARN|", wantLogs: 0},
		{name: "warn at none level", maxLevel: None, message: (*DLog).WarnMessage,
			want: "WARN|", wantLogs: 0},
		{name: "error at error level", maxLevel: Error, message: (*DLog).ErrorMessage,
			want: "ERROR|", wantLogs: 1},
		{name: "error at fatal level", maxLevel: Fatal, message: (*DLog).ErrorMessage,
			want: "ERROR|", wantLogs: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &behaviorLogger{}
			d := &DLog{logger: recorder, sourceProcess: source.Server,
				sourcePackage: source.Server, maxLevel: tt.maxLevel, hostname: "host-a"}
			got := tt.message(d, "Unable to read file(s)", errors.New("problem"))
			if !strings.HasPrefix(got, tt.want) || !strings.HasSuffix(got, "|Unable to read file(s)|problem") {
				t.Fatalf("message = %q, want %q...|Unable to read file(s)|problem", got, tt.want)
			}
			if len(recorder.logs) != tt.wantLogs {
				t.Fatalf("sink received %d logs (%q), want %d", len(recorder.logs), recorder.logs, tt.wantLogs)
			}
			if tt.wantLogs > 0 && recorder.logs[0] != got {
				t.Fatalf("sink log = %q, want the returned message %q", recorder.logs[0], got)
			}
		})
	}
}

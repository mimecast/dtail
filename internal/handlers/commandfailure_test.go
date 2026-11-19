package handlers

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/protocol"
)

// TestSessionCommandsReportFailures checks that a session command the server
// can not run is reported to the client with a hidden failed-command message
// after its diagnostic.
func TestSessionCommandsReportFailures(t *testing.T) {
	tests := []struct {
		name string
		run  func(*ServerHandler)
		want string
		// diagnostic, if set, is part of the command's diagnostic message.
		diagnostic string
	}{
		{
			name: "invalid mapreduce query",
			run: func(h *ServerHandler) {
				if err := h.dispatchCommand(context.Background(), []string{"map", "garbage"}, 2); err != nil {
					t.Errorf("dispatch map: %v", err)
				}
			},
			want: commandFailureMapQuery,
		},
		{
			// An older client does not reject a query selecting one field
			// twice itself; dserver must, rather than count it twice.
			name: "duplicate select field",
			run: func(h *ServerHandler) {
				args := []string{"map", "from", "STATS", "select", "count(x),count(x)"}
				if err := h.dispatchCommand(context.Background(), args, len(args)); err != nil {
					t.Errorf("dispatch map: %v", err)
				}
			},
			want:       commandFailureMapQuery,
			diagnostic: "'select' clause has 'count(x)' more than once",
		},
		{
			name: "undecodable command",
			run: func(h *ServerHandler) {
				h.handleCommand("protocol " + protocol.ProtocolCompat + " base64 !!!")
			},
			want: commandFailureProtocol,
		},
		{
			name: "rejected command options",
			run: func(h *ServerHandler) {
				h.handleCommand(encodedTestCommand("cat:invalid-option test.log ."))
			},
			want: commandFailureRejected,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newMapTestHandler(t)
			readServerMessage(t, handler.serverMessages) // Initial capability advertisement.
			go tt.run(handler)

			diagnostic := readServerMessage(t, handler.serverMessages)
			if !strings.Contains(diagnostic, tt.diagnostic) {
				t.Fatalf("diagnostic = %q, want it to contain %q", diagnostic, tt.diagnostic)
			}
			want := protocol.HiddenCommandFailedPrefix + tt.want + "\n"
			if message := readServerMessage(t, handler.serverMessages); message != want {
				t.Fatalf("message after the diagnostic = %q, want %q", message, want)
			}
		})
	}
}

// encodedTestCommand frames command like a client does.
func encodedTestCommand(command string) string {
	return "protocol " + protocol.ProtocolCompat + " base64 " + base64.StdEncoding.EncodeToString([]byte(command))
}

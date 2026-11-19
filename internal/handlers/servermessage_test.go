package handlers

// Tests that the diagnostics a server sends its client keep their text when
// the server's log level filters them out. dserver words these messages with
// its logger, whose Warn and Error return "" for a disabled level, so with
// --logLevel error the client used to print bare "SERVER|<host>|" lines.

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/session"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

// errorLevelLogger is a logger at error level with the MessageLogger
// capability, like dserver's logger at --logLevel error: Warn returns "" and
// logs nothing, WarnMessage logs nothing but returns the text, and Error and
// ErrorMessage log and return the text.
type errorLevelLogger struct {
	logging.NopLogger
	mu     sync.Mutex
	logged []string
}

var _ logging.MessageLogger = (*errorLevelLogger)(nil)

func (l *errorLevelLogger) Error(args ...any) string        { return l.log("ERROR", args) }
func (l *errorLevelLogger) ErrorMessage(args ...any) string { return l.log("ERROR", args) }
func (l *errorLevelLogger) WarnMessage(args ...any) string  { return formatTestDiagnostic("WARN", args) }

func (l *errorLevelLogger) log(level string, args []any) string {
	message := formatTestDiagnostic(level, args)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logged = append(l.logged, message)
	return message
}

func (l *errorLevelLogger) loggedLines() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.logged, "\n")
}

func formatTestDiagnostic(level string, args []any) string {
	return level + protocol.FieldSeparator() + protocol.EncodeDiagnosticDetails(args)
}

// newServerMessageTestHandler returns a dserver-like ServerHandler (no
// serverless output) that may read every file and logs through logger.
func newServerMessageTestHandler(t *testing.T, logger logging.Logger) *ServerHandler {
	t.Helper()
	lookup := func(string) ([]string, error) { return []string{"readfiles:.*"}, nil }
	user, err := userserver.New("server-message-user", "127.0.0.1", lookup, handlerTestLogger)
	if err != nil {
		t.Fatalf("new user: %v", err)
	}
	handler, err := NewServerHandler(context.Background(), user, Dependencies{
		CatLimiter:   make(chan struct{}, 4),
		TailLimiter:  make(chan struct{}, 4),
		ServerConfig: &config.ServerConfig{MapreduceLogFormat: "default", AuthKeyEnabled: true},
		AuthKeyStore: authkey.New(time.Hour, 5),
		Loggers:      HandlerLoggers{Diagnostics: logger, Reader: logger},
	})
	if err != nil {
		t.Fatalf("NewServerHandler: %v", err)
	}
	handler.readTimings.globRetryInterval = time.Millisecond
	return handler
}

// serverMessageTestLoggers are loggers that filter warnings out: one at error
// level with the MessageLogger capability, as dserver's logger at --logLevel
// error, and one without it that logs nothing.
var serverMessageTestLoggers = []struct {
	name      string
	newLogger func() logging.Logger
}{
	{name: "error level", newLogger: func() logging.Logger { return &errorLevelLogger{} }},
	{name: "no MessageLogger capability", newLogger: func() logging.Logger { return logging.NopLogger{} }},
}

// TestReadMessagesKeepTheirTextWhenTheLogLevelFiltersThem reads a missing
// file, with framed output, and checks that the client gets the text of its
// warnings and no empty server message.
func TestReadMessagesKeepTheirTextWhenTheLogLevelFiltersThem(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.log")
	commands, err := session.Spec{Mode: omode.CatClient, Files: []string{missing}}.Commands()
	if err != nil {
		t.Fatalf("build commands: %v", err)
	}
	want := []string{"WARN|", "|Unable to read file(s), check server logs", "|Giving up to read file(s)"}
	for _, logger := range serverMessageTestLoggers {
		t.Run(logger.name, func(t *testing.T) {
			handler := newServerMessageTestHandler(t, logger.newLogger())
			output := runLegacyCommandsOn(t, handler, commands)
			for _, text := range want {
				if !strings.Contains(output, text) {
					t.Fatalf("client output misses %q:\n%q", text, output)
				}
			}
			assertNoEmptyServerMessage(t, output)
		})
	}
}

// TestCommandMessagesKeepTheirTextWhenTheLogLevelFiltersThem sends commands
// the dispatcher rejects and checks the text of the message the client gets.
func TestCommandMessagesKeepTheirTextWhenTheLogLevelFiltersThem(t *testing.T) {
	tests := []struct {
		name  string
		frame string
		want  string
	}{
		{name: "unknown command", frame: encodeTestCommand("bogus: x"),
			want: "ERROR|server-message-user@127.0.0.1|Received unknown user command|bogus"},
		{name: "protocol mismatch", frame: "protocol 0.1 base64 Ym9ndXM=;",
			want: "ERROR|server-message-user@127.0.0.1|the DTail server protocol version"},
		{name: "no protocol", frame: "bogus;",
			want: "ERROR|server-message-user@127.0.0.1|unable to determine protocol version"},
	}
	for _, logger := range serverMessageTestLoggers {
		for _, tt := range tests {
			t.Run(logger.name+"/"+tt.name, func(t *testing.T) {
				handler := newServerMessageTestHandler(t, logger.newLogger())
				defer handler.Shutdown()
				// An unknown command ends the session, which waits in Write
				// for the client's close acknowledgement given below.
				written := make(chan error, 1)
				go func() {
					_, err := handler.Write([]byte(tt.frame))
					written <- err
				}()
				got := readServerMessageContaining(t, handler.serverMessages, "ERROR|")
				handler.acknowledgeClose()
				if err := <-written; err != nil {
					t.Fatalf("write command: %v", err)
				}
				if !strings.HasPrefix(got, tt.want) {
					t.Fatalf("server message = %q, want prefix %q", got, tt.want)
				}
			})
		}
	}
}

// readServerMessageContaining returns the first message of messages that
// contains text, skipping others such as the capabilities announcement.
func readServerMessageContaining(t *testing.T, messages <-chan string, text string) string {
	t.Helper()
	for {
		if message := readServerMessage(t, messages); strings.Contains(message, text) {
			return message
		}
	}
}

// TestServerMessagesAreLoggedAtTheirLevel checks that a message the client
// gets is still logged only when the server's level allows it: the missing
// file's warnings are not logged at error level, while its error is.
func TestServerMessagesAreLoggedAtTheirLevel(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.log")
	commands, err := session.Spec{Mode: omode.CatClient, Files: []string{missing}}.Commands()
	if err != nil {
		t.Fatalf("build commands: %v", err)
	}
	logger := &errorLevelLogger{}
	runLegacyCommandsOn(t, newServerMessageTestHandler(t, logger), commands)
	logged := logger.loggedLines()
	if strings.Contains(logged, "WARN|") {
		t.Fatalf("logger at error level logged a warning:\n%s", logged)
	}
	if !strings.Contains(logged, "ERROR|") || !strings.Contains(logged, "No such file(s) to read") {
		t.Fatalf("logger at error level did not log the missing file's error:\n%s", logged)
	}
}

// assertNoEmptyServerMessage fails when output has a server message frame
// without text ("SERVER|<host>|" followed by the frame end).
func assertNoEmptyServerMessage(t *testing.T, output string) {
	t.Helper()
	for _, frame := range strings.Split(output, string([]byte{protocol.MessageDelimiter})) {
		frame = strings.TrimSuffix(frame, "\n")
		if !strings.HasPrefix(frame, protocol.ServerMessageID+protocol.FieldSeparator()) {
			continue
		}
		fields := strings.SplitN(frame, protocol.FieldSeparator(), 3)
		if len(fields) < 3 || strings.TrimSpace(fields[2]) == "" {
			t.Fatalf("empty server message %q in client output:\n%q", frame, output)
		}
	}
}

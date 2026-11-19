package clients

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/protocol"
)

const (
	// capsNew is the capabilities frame of a server reporting failures.
	capsNew = protocol.HiddenCapabilitiesPrefix + protocol.CapabilityQueryUpdateV1 + " " +
		protocol.CapabilityCommandFailureV1 + " " + protocol.CapabilityEscapedFramingV1
	// capsOld is the capabilities frame of a server reporting no failures.
	capsOld   = protocol.HiddenCapabilitiesPrefix + protocol.CapabilityQueryUpdateV1
	closeSync = ".syn close connection"
)

// oldServerWarning is the visible warning frame a server sends for a file
// it could not read (see readCommand.readFileIfPermissions), the only sign of
// the failure an old server without protocol.CapabilityCommandFailureV1
// gives.
var oldServerWarning = protocol.ServerMessageID + protocol.FieldSeparator() + "srv1" +
	protocol.FieldSeparator() + protocol.EncodeTimedDiagnostic(unreadableFileWarning)

// unreadableFileWarning is the warning of oldServerWarning.
var unreadableFileWarning = protocol.TimedDiagnostic{
	Level:     "WARN",
	Timestamp: "1016-120000",
	Details:   []any{"Unable to read file(s), check server logs"},
}

// TestSessionStatus covers how a connection's exit status follows from its
// handler's status, the failed commands its server reported, and whether the
// session was cut short without the client cancelling it.
func TestSessionStatus(t *testing.T) {
	t.Parallel()

	read := clientProfile{failedCommandStatus: ExitStatusFailedCommand, cutShortStatus: exitStatusCutShort}
	completed := handlers.SessionOutcome{Completed: true}
	noFile := handlers.SessionOutcome{Completed: true, Failures: []string{protocol.CommandFailureNoFile}}
	cutShortNoFile := handlers.SessionOutcome{Failures: []string{protocol.CommandFailureNoFile}}
	tests := []struct {
		name      string
		profile   clientProfile
		handler   handlers.Handler
		cancelled bool // the client ended the session itself
		want      int
		logged    bool // the client logged an error for a session cut short
	}{
		{name: "read client, no failure", profile: read,
			handler: &statusTestHandler{outcome: completed}, want: 0},
		{name: "read client, file read failure", profile: read,
			handler: &statusTestHandler{outcome: noFile}, want: ExitStatusFailedCommand},
		{name: "read client, other failed command", profile: read,
			handler: &statusTestHandler{outcome: handlers.SessionOutcome{Completed: true,
				Failures: []string{protocol.CommandFailureOutputTooLarge}}}, want: ExitStatusFailedCommand},
		{name: "read client, connection failure and file read failure", profile: read,
			handler: &statusTestHandler{status: 1, outcome: noFile}, want: ExitStatusFailedCommand},
		{name: "read client, higher handler status wins", profile: read,
			handler: &statusTestHandler{status: 3, outcome: noFile}, want: 3},
		{name: "read client, connection failure only", profile: read,
			handler: &statusTestHandler{status: 1}, want: 1},
		{name: "read client, handler without outcome", profile: read,
			handler: &retryTestHandler{}, want: 0},
		{name: "read client, cut short", profile: read,
			handler: &statusTestHandler{}, want: exitStatusCutShort, logged: true},
		// A client handler's status stays -1 while it reports no error.
		{name: "read client, cut short, initial handler status", profile: read,
			handler: &statusTestHandler{status: -1}, want: exitStatusCutShort, logged: true},
		{name: "read client, cut short after a file read failure", profile: read,
			handler: &statusTestHandler{outcome: cutShortNoFile}, want: ExitStatusFailedCommand, logged: true},
		{name: "read client, cancelled by the client", profile: read,
			handler: &statusTestHandler{}, cancelled: true, want: 0},
		{name: "read client, cancelled by the client after a file read failure", profile: read,
			handler: &statusTestHandler{outcome: cutShortNoFile}, cancelled: true, want: ExitStatusFailedCommand},
		{name: "read client, completed although cancelled", profile: read,
			handler: &statusTestHandler{outcome: completed}, cancelled: true, want: 0},
		{name: "other client, file read failure", handler: &statusTestHandler{outcome: noFile}, want: 0},
		{name: "other client, cut short", handler: &statusTestHandler{}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger := &reportHookLogger{}
			client := &baseClient{profile: tt.profile, loggers: LoggerDependencies{Client: logger}}
			if got := client.sessionStatus(tt.cancelled, tt.handler); got != tt.want {
				t.Fatalf("sessionStatus() = %d, want %d", got, tt.want)
			}
			if logged := logger.errors.Load() > 0; logged != tt.logged {
				t.Fatalf("sessionStatus() logged an error = %v, want %v", logged, tt.logged)
			}
		})
	}
}

// TestStartExitStatus runs clients whose connections receive the hidden
// messages a server ends a session with, through real client handlers, and
// checks the client's exit status: the highest of its connections.
func TestStartExitStatus(t *testing.T) {
	t.Parallel()

	noFile := protocol.HiddenCommandFailedPrefix + protocol.CommandFailureNoFile
	tooLarge := protocol.HiddenCommandFailedPrefix + protocol.CommandFailureOutputTooLarge
	ok := messageSession{messages: []string{capsNew, closeSync}}
	readFailure := messageSession{messages: []string{capsNew, noFile, closeSync}}
	connFailure := messageSession{messages: []string{capsNew}, serverError: "connection refused"}
	// A session the server ended without its close handshake, e.g. killed.
	cutShort := messageSession{messages: []string{capsNew}}
	tests := []struct {
		name     string
		profile  clientProfile
		sessions []messageSession
		// cancelled starts the client with a cancelled context, as after
		// SIGINT or SIGTERM.
		cancelled bool
		want      int
	}{
		{name: "dcat, file read", profile: readClientProfile(nil), sessions: []messageSession{ok}, want: 0},
		{name: "dcat, missing file", profile: readClientProfile(nil),
			sessions: []messageSession{readFailure}, want: ExitStatusFailedCommand},
		{name: "dcat, output too large", profile: readClientProfile(nil),
			sessions: []messageSession{{messages: []string{capsNew, tooLarge, closeSync}}},
			want:     ExitStatusFailedCommand},
		// An unsupported old server closes normally but cannot be trusted
		// to have run the read; the compatibility error sets status 1.
		{name: "dcat, unsupported old server", profile: readClientProfile(nil),
			sessions: []messageSession{{messages: []string{capsOld, oldServerWarning, closeSync}}}, want: 1},
		{name: "dcat, missing file on one of two servers", profile: readClientProfile(nil),
			sessions: []messageSession{ok, readFailure}, want: ExitStatusFailedCommand},
		{name: "dcat, connection failure", profile: readClientProfile(nil),
			sessions: []messageSession{ok, connFailure}, want: 1},
		{name: "dcat, connection failure and missing file", profile: readClientProfile(nil),
			sessions: []messageSession{connFailure, readFailure}, want: ExitStatusFailedCommand},
		{name: "dcat, cut short", profile: readClientProfile(nil),
			sessions: []messageSession{cutShort}, want: exitStatusCutShort},
		{name: "dcat, cut short on one of two servers", profile: readClientProfile(nil),
			sessions: []messageSession{ok, cutShort}, want: exitStatusCutShort},
		{name: "dcat, cut short after a missing file", profile: readClientProfile(nil),
			sessions: []messageSession{{messages: []string{capsNew, noFile}}}, want: ExitStatusFailedCommand},
		{name: "dcat, cut short on one server, missing file on another", profile: readClientProfile(nil),
			sessions: []messageSession{cutShort, readFailure}, want: ExitStatusFailedCommand},
		{name: "dcat, cancelled by the client", profile: readClientProfile(nil),
			sessions: []messageSession{cutShort}, cancelled: true, want: 0},
		// dtail's profile without its retry, which would reconnect forever.
		{name: "dtail, missing file", profile: clientHandlerProfile(nil, false),
			sessions: []messageSession{readFailure}, want: 0},
		{name: "dtail, cut short", profile: clientHandlerProfile(nil, false),
			sessions: []messageSession{cutShort}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &baseClient{mu: newBaseClientMu(), profile: tt.profile,
				stats: newTailStats(len(tt.sessions), nil, 0, nil)}
			for i, session := range tt.sessions {
				handler := tt.profile.newHandler(fmt.Sprintf("srv%d", i+1))
				client.connections = append(client.connections, &messageConnector{
					retryTestConnector: retryTestConnector{handler: handler}, t: t, session: session})
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancelled {
				cancel()
			}
			if got := client.Start(ctx, make(chan string)); got != tt.want {
				t.Fatalf("Start() status = %d, want %d", got, tt.want)
			}
		})
	}
}

// messageSession is what a messageConnector's session receives: the hidden
// messages of the server, then serverError as a connection error unless it
// is empty.
type messageSession struct {
	messages    []string
	serverError string
}

// messageConnector feeds its handler the messages of its session.
type messageConnector struct {
	retryTestConnector
	t       *testing.T
	session messageSession
}

var _ connectors.Connector = (*messageConnector)(nil)

func (c *messageConnector) Start(_ context.Context, cancel context.CancelFunc, _, _ chan struct{}) {
	defer cancel()
	handler := c.Handler()
	// Take the close acknowledgement the handler sends.
	go func() { _, _ = io.Copy(io.Discard, handler) }()
	defer handler.Shutdown()
	for _, message := range c.session.messages {
		if _, err := handler.Write(append([]byte(message), protocol.MessageDelimiter)); err != nil {
			c.t.Errorf("Write(%q) error = %v", message, err)
		}
	}
	if c.session.serverError != "" {
		handler.ReportServerError(c.session.serverError)
	}
}

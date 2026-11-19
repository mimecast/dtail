package clients

import (
	"github.com/mimecast/dtail/internal/clients/handlers"
)

// ExitStatusFailedCommand is the exit status of dcat and dgrep (grep's error
// status) when a server reported a failed command of their session through
// protocol.HiddenCommandFailedPrefix: a file read failure
// (protocol.IsFileReadFailure, e.g. a missing, not permitted or unreadable
// file), and any other reported reason too (e.g. a read command the server
// could not parse, or output too large for the session output buffer), as
// each means the output is missing data that was asked for. It is the same
// in serverless mode, whose in-process server reports failures the same way.
// Servers without protocol.CapabilityCommandFailureV1 report no failures, so
// with them the status stays the connection status. A connection status of
// 1 (a refused or unreachable server, a failed SSH handshake, an SSH session
// or shell that could not be opened, an --interactive-query SESSION START the
// server rejected or did not acknowledge, commands that could not be sent, a
// journal target the server does not support (no journal-v1), a path
// containing a space or starting with base64% the server cannot read (no
// encoded-read-path-v1), a failed serverless in-process session: every error
// ServerConnection.Start and Serverless.Start pass to
// Handler.ReportServerError; and a session cut short, see
// exitStatusCutShort) stays 1 unless such a report raises it; the client
// exits with the highest status of all its connections (see
// baseClient.runConnections).
const ExitStatusFailedCommand = 2

// exitStatusCutShort is the exit status of dcat and dgrep for a session that
// ended without the server's close handshake (handlers.SessionOutcome
// Completed is false) although the client did not end it itself: e.g. a
// dserver killed, crashed or shut down in the middle of a read, or a lost
// connection, so that the output may be incomplete. Servers of every protocol
// version end a session they completed with the handshake (".syn close
// connection", sent after all of the session's output; the in-process server
// of serverless mode too), so no capability gates this. A session the client
// ended itself keeps its status: SIGINT/SIGTERM cancel the context of
// Client.Start, and :quit and the end of the control TTY of an
// --interactive-query session cancel the context of runConnections. A
// connection that failed before (status 1, already reported) keeps 1, and a
// reported failed command (status 2) outweighs it.
const exitStatusCutShort = 1

// sessionOutcomeReporter is a client handler that knows how its session
// ended.
type sessionOutcomeReporter interface {
	Outcome() handlers.SessionOutcome
}

// sessionStatus returns the exit status of a connection whose session ran
// with handler, where endedByClient tells whether the client ended that
// session itself (see baseClient.runSession): the handler's status, raised to
// the profile's failedCommandStatus when that is set and the server reported
// a failed command, and to the profile's cutShortStatus when that is set and
// the session was cut short (see exitStatusCutShort) while the client did not
// end it. The reports are counted whether or not the session completed, and
// the highest status wins. A handler that cannot tell how its session ended
// keeps its status.
func (c *baseClient) sessionStatus(endedByClient bool, handler handlers.Handler) int {
	status := handler.Status()
	reporter, ok := handler.(sessionOutcomeReporter)
	if !ok {
		return status
	}
	outcome := reporter.Outcome()
	if c.profile.failedCommandStatus != 0 && len(outcome.Failures) > 0 {
		status = max(status, c.profile.failedCommandStatus)
	}
	if c.profile.cutShortStatus != 0 && !outcome.Completed && !endedByClient {
		c.reportCutShort(handler)
		status = max(status, c.profile.cutShortStatus)
	}
	return status
}

// reportCutShort logs that the session of handler was cut short, unless the
// handler already reported an error of its connection (a positive status;
// client handlers start with -1 and keep it while no error is reported),
// which then is why the session ended.
func (c *baseClient) reportCutShort(handler handlers.Handler) {
	if handler.Status() > 0 {
		return
	}
	c.clientLogger().Error(handler.Server(),
		"Session ended without the server's close handshake, the output may be incomplete")
}

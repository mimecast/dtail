package handlers

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	user "github.com/mimecast/dtail/internal/sessionuser"
)

// HealthHandler is for the remote health check.
type HealthHandler struct {
	*baseHandler
	// shutdownStarted claims the session's one shutdown, started by its
	// health command (handleHealthCommand) or by a rejected command frame
	// (frameRejected, versionRejected). The client's close
	// acknowledgement, a command frame too, then only completes that
	// shutdown instead of starting another, whose flush would fail once the
	// first one canceled the commands and report that after the close
	// handshake. It also makes the dispatcher drop later frames of another
	// protocol version (sessionClosingCb) and later frames it rejects
	// (rejectionsDroppedCb) without an error, and the handler later
	// commands (ignoreAfterEnd).
	shutdownStarted atomic.Bool
}

// NewHealthHandler returns the server handler.
func NewHealthHandler(ctx context.Context, user *user.User, maxFrameSize int, hostname string,
	logger logging.Logger) (*HealthHandler, error) {
	logger = logging.OrNop(logger)
	logger.Debug(user, "Creating new server health handler")
	if user == nil {
		return nil, fmt.Errorf("create health handler: user must not be nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("create health handler: context must not be nil")
	}

	h := &HealthHandler{
		baseHandler: newBaseHandler(ctx, baseHandlerConfig{
			logger:              logger,
			user:                user,
			maxCommandFrameSize: maxFrameSize,
		}),
	}
	h.handleCommandCb = h.handleHealthCommand
	h.frameRejectedCb = h.frameRejected
	h.versionRejectedCb = h.versionRejected
	h.sessionClosingCb = h.shutdownStarted.Load
	h.rejectionsDroppedCb = h.shutdownStarted.Load

	s := strings.Split(hostname, ".")
	h.hostname = s[0]
	return h, nil
}

// frameRejected ends a health session whose command frame the dispatcher
// rejected before any handler ran (commandDispatcher.frameRejected:
// undecodable frame, invalid timeout prefix or unparsable options). A health
// session serves one health command and no handler runs for such a frame, so
// without this the session stayed open until the client gave up. The
// rejection's error is already queued, so the close handshake follows it.
func (h *HealthHandler) frameRejected() {
	h.startEnd(h.shutdown)
}

// versionRejected ends a health session after a command frame of another
// protocol version was rejected (commandDispatcher.rejectProtocolVersion),
// like frameRejected, but without the close handshake
// (closeWithoutHandshake), which such a client may not recognize or
// acknowledge. The session of a legacy client (protocol <= 3, an unparsable
// version or no protocol prefix) stays open instead. A protocol 4.x client
// is ended like a newer one, unlike in ServerHandler.versionRejected:
// dtailhealth does not reconnect, and a health check of a 4.x client must
// fail at once rather than wait until the client gives up
// (protocol4Client). The health client still reads the mismatch error and
// reports the check as failed.
func (h *HealthHandler) versionRejected(client mismatchedClient) {
	if client == legacyClient {
		h.keepMismatchedSessionOpen()
		return
	}
	h.startEnd(h.closeWithoutHandshake)
}

// startEnd claims the session's one shutdown and runs end, which closes the
// session, on a goroutine of its own: the health command and the rejection
// hooks run on the command input goroutine, which must stay free to handle
// the client's close acknowledgement. Run inline, the shutdown would wait
// for an acknowledgement that the goroutine could only read after it, so
// only the client's disconnect or the 5 s acknowledgement timeout ended it.
// end gets the connection's context, which the client's disconnect cancels.
func (h *HealthHandler) startEnd(end func(context.Context)) {
	if !h.shutdownStarted.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer recoverHandlerPanic(h.Logger(), h.user, "health shutdown", h.abortAfterPanic)
		end(h.connCtx)
	}()
}

// handleHealthCommand answers a health command with OK and then ends the
// session, as a health session serves one health command. A command of
// another name is reported as unknown and ends the session too, except an
// auth-key registration (isAuthKeyCommand), which is ignored. Once the
// shutdown was claimed, only close acknowledgements are handled
// (ignoreAfterEnd).
func (h *HealthHandler) handleHealthCommand(ctx context.Context,
	ltx lcontext.LContext, argc int, args []string, commandName string) {

	h.Logger().Debug(h.user, "Handling health command", argc, args)
	if isAuthKeyCommand(commandName) {
		h.ignoreAuthKey(ctx)
		return
	}
	if h.ignoreAfterEnd(ctx, argc, commandName) {
		return
	}
	switch commandName {
	case "health":
		h.send(h.serverMessages, "OK")
	case ".ack":
		h.handleAckCommand(argc, args)
	default:
		h.send(h.serverMessages, logging.ErrorMessage(h.Logger(), h.user,
			"Received unknown health command", commandName, argc, args))
	}
	// The shutdown waits for the client's close acknowledgement, a command
	// frame the input goroutine reads only after this call returned, so it
	// runs on a goroutine of its own (startEnd), bounded by the connection's
	// context rather than this command's, which is released right away.
	h.startEnd(h.shutdown)
	cancelCommandContext(ctx)
}

// ignoreAfterEnd drops a command that arrives after the session's shutdown
// was claimed, other than a well-formed ".ack" (at least three words, which
// handleAckCommand would otherwise warn about), and reports whether it did. The shutdown
// runs on a goroutine of its own (startEnd), so the input goroutine goes on
// with the frames the client sent after the one that ended the session;
// answering them would put an answer (e.g. an OK after an unknown command,
// or a warning about a malformed ".ack") before the close handshake. Frames
// the dispatcher rejects before any handler runs are dropped there
// (rejectionsDroppedCb). A health session serves one command, as when
// the shutdown still ran inline and those frames were read only after it.
func (h *HealthHandler) ignoreAfterEnd(ctx context.Context, argc int, commandName string) bool {
	if !h.shutdownStarted.Load() || (commandName == ".ack" && argc >= 3) {
		return false
	}
	h.Logger().Debug(h.user, "Ignoring health session command after its end", commandName)
	cancelCommandContext(ctx)
	return true
}

// ignoreAuthKey drops an AUTHKEY command without an answer and without
// claiming the session's shutdown, so the health command the client sends
// after it is still answered OK. dtailhealth registered the public key of
// its auth key (<auth key>.pub) before its health command until its
// client stopped registering one (clients.NewHealthClient sets NoAuthKey),
// and dtailhealth binaries of protocol 5 from before that still do; the
// health handler used to report it as unknown and end the session, so a
// healthy server was reported CRITICAL. Nothing is registered: the health
// user authenticates by password and may not register keys
// (ServerHandler.handleAuthKeyCommand refuses it as "unsupported user").
// There is no AUTHKEY reply either, which the health client would not read.
func (h *HealthHandler) ignoreAuthKey(ctx context.Context) {
	h.Logger().Debug(h.user, "Ignoring AUTHKEY in a health session")
	cancelCommandContext(ctx)
}

// isAuthKeyCommand reports whether commandName names an auth-key
// registration, in either case the ServerHandler accepts.
func isAuthKeyCommand(commandName string) bool {
	return commandName == "AUTHKEY" || commandName == "authkey"
}

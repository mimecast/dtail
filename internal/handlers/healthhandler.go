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
	// protocol version without an error (sessionClosingCb).
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
// acknowledge. It does not run for a legacy client (protocol <= 3, an
// unparsable version or no protocol prefix), whose session stays open. The
// health client still reads the mismatch error and reports the check as
// failed.
func (h *HealthHandler) versionRejected() {
	h.startEnd(h.closeWithoutHandshake)
}

// startEnd claims the session's one shutdown and runs end, which closes the
// session, on a goroutine of its own: the rejection hooks run on the command
// input goroutine, which must stay free to handle the client's close
// acknowledgement.
func (h *HealthHandler) startEnd(end func(context.Context)) {
	if !h.shutdownStarted.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer recoverHandlerPanic(h.Logger(), h.user, "health shutdown", h.abortAfterPanic)
		end(h.connCtx)
	}()
}

func (h *HealthHandler) handleHealthCommand(ctx context.Context,
	ltx lcontext.LContext, argc int, args []string, commandName string) {

	h.Logger().Debug(h.user, "Handling health command", argc, args)
	switch commandName {
	case "health":
		h.send(h.serverMessages, "OK")
	case ".ack":
		h.handleAckCommand(argc, args)
	default:
		h.send(h.serverMessages, logging.ErrorMessage(h.Logger(), h.user,
			"Received unknown health command", commandName, argc, args))
	}
	if h.shutdownStarted.CompareAndSwap(false, true) {
		h.shutdown(ctx)
	}
	// shutdown drains the response before canceling the handler's command root.
	// Release the command-local child as well for callers that supply their own.
	cancelCommandContext(ctx)
}

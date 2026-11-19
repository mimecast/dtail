package handlers

import (
	"context"
	"fmt"
	"strings"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	user "github.com/mimecast/dtail/internal/sessionuser"
)

// HealthHandler is for the remote health check.
type HealthHandler struct {
	*baseHandler
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

	h := HealthHandler{
		baseHandler: newBaseHandler(ctx, baseHandlerConfig{
			logger:              logger,
			user:                user,
			maxCommandFrameSize: maxFrameSize,
		}),
	}
	h.handleCommandCb = h.handleHealthCommand

	s := strings.Split(hostname, ".")
	h.hostname = s[0]
	return &h, nil
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
		h.send(h.serverMessages, h.Logger().Error(h.user,
			"Received unknown health command", commandName, argc, args))
	}
	h.shutdown(ctx)
	// shutdown drains the response before canceling the handler's command root.
	// Release the command-local child as well for callers that supply their own.
	cancelCommandContext(ctx)
}

package handlers

import (
	"context"
	"strings"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	user "github.com/mimecast/dtail/internal/user/server"
)

// HealthHandler is for the remote health check.
type HealthHandler struct {
	baseHandler
}

// NewHealthHandler returns the server handler.
func NewHealthHandler(user *user.User) *HealthHandler {
	dlog.Server.Debug(user, "Creating new server health handler")
	h := HealthHandler{
		baseHandler: baseHandler{
			done:             internal.NewDone(),
			lines:            make(chan *line.Line, 100),
			serverMessages:   make(chan string, 10),
			maprMessages:     make(chan string, 10),
			ackCloseReceived: make(chan struct{}),
			user:             user,
		},
	}
	h.handleCommandCb = h.handleHealthCommand

	fqdn, err := config.Hostname()
	if err != nil {
		dlog.Server.FatalPanic(err)
	}
	s := strings.Split(fqdn, ".")
	h.hostname = s[0]
	return &h
}

func (h *HealthHandler) handleHealthCommand(ctx context.Context,
	ltx lcontext.LContext, argc int, args []string, commandName string) {

	dlog.Server.Debug(h.user, "Handling health command", argc, args)
	switch commandName {
	case "health":
		h.send(h.serverMessages, "OK")
	case ".ack":
		h.handleAckCommand(argc, args)
	default:
		h.send(h.serverMessages, dlog.Server.Error(h.user,
			"Received unknown health command", commandName, argc, args))
	}
	h.shutdown()
}

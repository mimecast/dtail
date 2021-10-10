package handlers

import (
	"context"
	"os"
	"strings"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/omode"
	user "github.com/mimecast/dtail/internal/user/server"
)

// ServerHandler implements the Reader and Writer interfaces to handle
// the Bi-directional communication between SSH client and server.
// This handler implements the handler of the SSH server.
type ServerHandler struct {
	baseHandler
	catLimiter  chan struct{}
	tailLimiter chan struct{}
	regex       string
}

// NewServerHandler returns the server handler.
func NewServerHandler(user *user.User, catLimiter,
	tailLimiter chan struct{}) *ServerHandler {

	dlog.Server.Debug(user, "Creating new server handler")
	h := ServerHandler{
		baseHandler: baseHandler{
			done:             internal.NewDone(),
			lines:            make(chan line.Line, 100),
			serverMessages:   make(chan string, 10),
			maprMessages:     make(chan string, 10),
			ackCloseReceived: make(chan struct{}),
			user:             user,
		},
		catLimiter:  catLimiter,
		tailLimiter: tailLimiter,
		regex:       ".",
	}
	h.handleCommandCb = h.handleUserCommand

	fqdn, err := os.Hostname()
	if err != nil {
		dlog.Server.FatalPanic(err)
	}

	s := strings.Split(fqdn, ".")
	h.hostname = s[0]

	return &h
}

func (h *ServerHandler) handleUserCommand(ctx context.Context, argc int,
	args []string, commandName string) {

	dlog.Server.Debug(h.user, "Handling user command", argc, args)
	h.incrementActiveCommands()
	commandFinished := func() {
		if h.decrementActiveCommands() == 0 {
			h.shutdown()
		}
	}

	switch commandName {
	case "grep", "cat":
		command := newReadCommand(h, omode.CatClient)
		go func() {
			command.Start(ctx, argc, args, 1)
			commandFinished()
		}()
	case "tail":
		command := newReadCommand(h, omode.TailClient)
		go func() {
			command.Start(ctx, argc, args, 10)
			commandFinished()
		}()
	case "map":
		command, aggregate, err := newMapCommand(h, argc, args)
		if err != nil {
			h.send(h.serverMessages, err.Error())
			dlog.Server.Error(h.user, err)
			commandFinished()
			return
		}
		h.aggregate = aggregate
		go func() {
			command.Start(ctx, h.maprMessages)
			commandFinished()
		}()
	case ".ack":
		h.handleAckCommand(argc, args)
		commandFinished()
	default:
		h.send(h.serverMessages, dlog.Server.Error(h.user,
			"Received unknown user command", commandName, argc, args))
		commandFinished()
	}
}

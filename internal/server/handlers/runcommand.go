package handlers

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/io/run"
)

type runCommand struct {
	server *ServerHandler
	run    run.Run
}

func newRunCommand(server *ServerHandler) runCommand {
	return runCommand{
		server: server,
	}
}

func (r runCommand) Start(ctx context.Context, argc int, args []string) {
	if argc < 2 {
		r.server.sendServerMessage(logger.Warn(r.server.user, commandParseWarning, args, argc))
		return
	}
	commands := strings.Split(strings.Join(args[1:], " "), ";")
	r.start(ctx, commands)
}

func (r runCommand) start(ctx context.Context, commands []string) {
	for _, command := range commands {
		command = strings.TrimSpace(command)
		if len(command) == 0 {
			continue
		}
		splitted := strings.Split(command, " ")
		path := splitted[0]
		args := splitted[1:]

		qualifiedPath, err := exec.LookPath(path)
		if err != nil {
			logger.Error(r.server.user, err)
			r.server.sendServerMessage(logger.Error(r.server.user, "Unable to execute command(s), check server logs"))
			r.server.sendServerMessage(".run exitstatus 255")
			return
		}

		if !r.server.user.HasFilePermission(qualifiedPath, "runcommands") {
			logger.Error(r.server.user, "No permission to execute path", qualifiedPath)
			r.server.sendServerMessage(logger.Error(r.server.user, "Unable to execute command(s), check server logs"))
			r.server.sendServerMessage(".run exitstatus 255")
			return
		}

		r.run = run.New(qualifiedPath, args)
		pid, ec, _ := r.run.Start(ctx, r.server.lines)

		r.server.sendServerMessage(fmt.Sprintf(".run exitstatus %d", ec))
		r.server.sendServerMessage(logger.Info(fmt.Sprintf("Process %d exited with status %d", pid, ec)))
	}
}

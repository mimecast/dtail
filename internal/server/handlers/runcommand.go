package handlers

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/config"
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

	command := strings.Join(args[1:], " ")
	if strings.Contains(command, ";") {
		r.startScript(ctx, command)
		return
	}

	r.start(ctx, strings.TrimSpace(command))
}

func (r runCommand) startScript(ctx context.Context, script string) {
	if _, err := os.Stat(config.Common.TmpDir); os.IsNotExist(err) {
		logger.Error(r.server.user, err)
		r.server.sendServerMessage(logger.Error(r.server.user, "Unable to execute command(s), check server logs"))
		return
	}

	timestamp := time.Now().UnixNano()
	scriptPath := fmt.Sprintf("%s/%s_%v.sh", config.Common.TmpDir, r.server.user.Name, timestamp)

	// TODO: On dserver startup delete all previously written scripts (there might be left overs due to a crash or so)
	logger.Debug(r.server.user, "Writing temp script", scriptPath)

	script = fmt.Sprintf("#!/bin/sh\n%s", script)
	if err := ioutil.WriteFile(scriptPath, []byte(script), 0700); err != nil {
		logger.Error(r.server.user, err)
		r.server.sendServerMessage(logger.Error(r.server.user, "Unable to execute command(s), check server logs"))
		return
	}

	r.start(ctx, scriptPath)
	os.Remove(scriptPath)
}

func (r runCommand) start(ctx context.Context, command string) {
	if len(command) == 0 {
		return
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

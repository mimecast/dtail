package handlers

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"sync"
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

func (r runCommand) StartBackground(ctx context.Context, wg *sync.WaitGroup, argc int, args, outerArgs []string) error {
	if argc < 2 {
		return fmt.Errorf("%s: args:%v argc:%d", commandParseWarning, args, argc)
	}

	ec := make(chan int, 1)
	var pid int
	var err error

	command := strings.Join(args[1:], " ")
	if strings.Contains(command, ";") || strings.Contains(command, "\n") {
		if pid, err = r.startScript(ctx, wg, ec, command, outerArgs); err != nil {
			r.server.sendServerMessage(".run exitstatus 255")
			return err
		}
		return nil
	}

	if pid, err = r.start(ctx, wg, ec, strings.TrimSpace(command), outerArgs); err != nil {
		r.server.sendServerMessage(".run exitstatus 255")
		return err
	}

	exitCode := <-ec
	r.server.sendServerMessage(fmt.Sprintf(".run exitstatus %d", exitCode))
	r.server.sendServerMessage(logger.Info(fmt.Sprintf("Process %d exited with status %d", pid, exitCode)))

	return nil
}

func (r runCommand) startScript(ctx context.Context, wg *sync.WaitGroup, ec chan<- int, script string, outerArgs []string) (int, error) {
	if _, err := os.Stat(config.Common.TmpDir); os.IsNotExist(err) {
		return -1, err
	}

	timestamp := time.Now().UnixNano()
	scriptPath := fmt.Sprintf("%s/%s_%v.sh", config.Common.TmpDir, r.server.user.Name, timestamp)

	// TODO: On dserver startup delete all previously written scripts (there might be left overs due to a crash or so)
	logger.Debug(r.server.user, "Writing temp script", scriptPath)

	script = fmt.Sprintf("#!/bin/sh\n%s", script)
	if err := ioutil.WriteFile(scriptPath, []byte(script), 0700); err != nil {
		return -1, err
	}

	pid, err := r.start(ctx, wg, ec, scriptPath, outerArgs)
	go func() {
		wg.Wait()
		logger.Debug("Deleting script", scriptPath)
		os.Remove(scriptPath)
	}()

	return pid, err
}

func (r runCommand) start(ctx context.Context, wg *sync.WaitGroup, ec chan<- int, command string, outerArgs []string) (int, error) {
	if len(command) == 0 {
		return -1, errors.New("Empty command provided")
	}

	splitted := strings.Split(command, " ")
	path := splitted[0]
	args := splitted[1:]
	args = append(args, outerArgs...)

	qualifiedPath, err := exec.LookPath(path)
	if err != nil {
		return -1, err
	}

	if !r.server.user.HasFilePermission(qualifiedPath, "runcommands") {
		return -1, fmt.Errorf("No permission to execute path: %s", qualifiedPath)
	}

	r.run = run.New(qualifiedPath, args)
	pid, err := r.run.StartBackground(ctx, wg, ec, r.server.lines)
	if err != nil {
		return pid, err
	}
	return pid, nil
}

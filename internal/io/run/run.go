package run

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/logger"
)

// Run is for execute a command.
type Run struct {
	commandPath string
	args        []string
	cmd         *exec.Cmd
}

// New returns a new command runner.
func New(commandPath string, args []string) Run {
	return Run{
		commandPath: commandPath,
		args:        args,
	}
}

// Start running the command.
func (r Run) Start(ctx context.Context, lines chan<- line.Line) (pid int, ec int, err error) {
	done := make(chan struct{})
	defer close(done)

	ec = -1
	pid = -1

	if len(r.args) > 0 {
		logger.Debug(r.commandPath, strings.Join(r.args, " "))
		r.cmd = exec.CommandContext(ctx, r.commandPath, strings.Join(r.args, " "))
	} else {
		logger.Debug(r.commandPath)
		r.cmd = exec.CommandContext(ctx, r.commandPath)
	}

	stdoutPipe, myErr := r.cmd.StdoutPipe()
	if err != nil {
		err = myErr
		return
	}

	stderrPipe, myErr := r.cmd.StderrPipe()
	if myErr != nil {
		err = myErr
		return
	}

	if myErr := r.cmd.Start(); err != nil {
		err = myErr
		return
	}

	pid = r.cmd.Process.Pid
	ec = 0

	var wg sync.WaitGroup
	wg.Add(2)

	go r.pipeToLines(done, &wg, pid, stdoutPipe, "STDOUT", lines)
	go r.pipeToLines(done, &wg, pid, stderrPipe, "STDERR", lines)

	if err = r.cmd.Wait(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			ec = exitError.ExitCode()
		}
	}

	return
}

func (r Run) pipeToLines(done chan struct{}, wg *sync.WaitGroup, pid int, reader io.Reader, what string, lines chan<- line.Line) {
	defer wg.Done()
	bufReader := bufio.NewReader(reader)

	for {
		lineStr, err := bufReader.ReadString('\n')
		for err == nil {
			lines <- line.Line{
				Content:         []byte(lineStr),
				Count:           uint64(pid),
				TransmittedPerc: 100,
				SourceID:        what,
			}
			lineStr, err = bufReader.ReadString('\n')
		}
		select {
		case <-done:
			return
		default:
		}
		time.Sleep(time.Millisecond * 10)
	}
}

package run

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/logger"
)

// Run is for execute a command.
type Run struct {
	command      string
	args         []string
	cmd          *exec.Cmd
	pgroupKilled chan struct{}
}

// New returns a new command runner.
func New(command string, args []string) Run {
	return Run{
		command:      command,
		args:         args,
		pgroupKilled: make(chan struct{}),
	}
}

// StartBackground starts running the command in background.
func (r Run) StartBackground(ctx context.Context, wg *sync.WaitGroup, ec chan<- int, lines chan<- line.Line) (pid int, err error) {
	pid = -1

	if len(r.args) > 0 {
		logger.Debug(r.command, r.args, " ")
		r.cmd = exec.CommandContext(ctx, r.command, r.args...)
	} else {
		logger.Debug(r.command)
		r.cmd = exec.CommandContext(ctx, r.command)
	}

	// Create a new process group, so that kill() will only kill this command + pgroup.
	r.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdoutPipe, myErr := r.cmd.StdoutPipe()
	if err != nil {
		wg.Done()
		err = myErr
		return
	}

	stderrPipe, myErr := r.cmd.StderrPipe()
	if myErr != nil {
		wg.Done()
		err = myErr
		return
	}

	if myErr := r.cmd.Start(); err != nil {
		wg.Done()
		err = myErr
		return
	}

	if r.cmd.Process != nil {
		pid = r.cmd.Process.Pid
	}

	commandExited := make(chan struct{})

	var pipeWg sync.WaitGroup
	pipeWg.Add(2)

	go r.killPgroup(ctx, commandExited, pid)
	go r.pipeToLines(commandExited, &pipeWg, pid, stdoutPipe, "STDOUT", lines)
	go r.pipeToLines(commandExited, &pipeWg, pid, stderrPipe, "STDERR", lines)

	go func() {
		exitCode := 255
		if waitErr := r.cmd.Wait(); waitErr != nil {
			if exitError, ok := waitErr.(*exec.ExitError); ok {
				exitCode = exitError.ExitCode()
			}
		}
		ec <- exitCode

		// Tell pipes we are done
		close(commandExited)
		// Wait for process group to be killed
		<-r.pgroupKilled
		// Wait for the pipes to flush the contents
		pipeWg.Wait()
		// Now the job is truly done
		wg.Done()
	}()

	return
}

func (r Run) pipeToLines(commandExited chan struct{}, wg *sync.WaitGroup, pid int, reader io.Reader, what string, lines chan<- line.Line) {
	defer wg.Done()
	bufReader := bufio.NewReader(reader)

	for {
		time.Sleep(time.Millisecond * 10)
		lineStr, err := bufReader.ReadString('\n')

		if err != nil {
			select {
			case <-commandExited:
				return
			}
			continue
		}

		newLine := line.Line{
			Content:         []byte(lineStr),
			Count:           uint64(pid),
			TransmittedPerc: 100,
			SourceID:        what,
		}

		select {
		case lines <- newLine:
		case <-commandExited:
			return
		}
	}
}

func (r Run) killPgroup(ctx context.Context, commandExited chan struct{}, pid int) {
	if pid == -1 {
		close(r.pgroupKilled)
		return
	}

	if pgid, err := syscall.Getpgid(pid); err == nil {
		// Kill process group when done
		select {
		case <-ctx.Done():
		case <-commandExited:
		}
		syscall.Kill(-pgid, syscall.SIGKILL)
		close(r.pgroupKilled)
	}
}

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

// Start running the command.
func (r Run) Start(ctx context.Context, lines chan<- line.Line) (pid int, ec int, err error) {
	done := make(chan struct{})
	defer close(done)

	ec = 255
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

	if r.cmd.Process != nil {
		pid = r.cmd.Process.Pid
		ec = 0
	}
	go r.killPgroup(ctx, done, pid)

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

// PgroupKilled identifies whether all subprocesses are killed or not.
func (r Run) PgroupKilled() <-chan struct{} {
	return r.pgroupKilled
}

func (r Run) killPgroup(ctx context.Context, done chan struct{}, pid int) {
	if pid == -1 {
		close(r.pgroupKilled)
		return
	}

	if pgid, err := syscall.Getpgid(pid); err == nil {
		// Kill process group when done
		select {
		case <-ctx.Done():
		case <-done:
		}
		syscall.Kill(-pgid, syscall.SIGKILL)
		close(r.pgroupKilled)
	}
}

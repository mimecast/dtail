package clients

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
)

const interactiveControlTimeout = 2 * time.Second

type interactiveCommand struct {
	spec SessionSpec
	kind string
	next config.Args
}

type interactiveReloadState struct {
	conn               connectors.Connector
	spec               SessionSpec
	rollbackGeneration uint64
}

func (c *baseClient) startInteractiveControl(ctx context.Context, statsCh <-chan string) int {
	controlTTY, err := os.OpenFile(c.ControlTTYPath, os.O_RDWR, 0)
	if err != nil {
		c.clientLogger().Error("Unable to open interactive query control TTY", c.ControlTTYPath, err)
		return 1
	}
	defer func() {
		if closeErr := controlTTY.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			c.clientLogger().Debug("Unable to close interactive query control TTY", closeErr)
		}
	}()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	statusCh := make(chan int, 1)
	go func() {
		statusCh <- c.runConnections(runCtx, statsCh)
	}()

	controlErrCh := make(chan error, 1)
	go func() {
		controlErrCh <- c.runInteractiveControl(runCtx, cancel, controlTTY)
	}()

	select {
	case status := <-statusCh:
		cancel()
		<-controlErrCh
		return status
	case controlErr := <-controlErrCh:
		if controlErr != nil {
			c.clientLogger().Warn("Interactive query control stopped", controlErr)
		}
		cancel()
		return <-statusCh
	}
}

func (c *baseClient) runInteractiveControl(ctx context.Context, cancel context.CancelFunc, tty *os.File) error {
	if _, err := fmt.Fprintf(tty,
		"Interactive query control enabled. Commands: :reload <flags>, :show, :help, :quit\n"); err != nil {
		return err
	}

	reader := bufio.NewScanner(tty)
	reader.Buffer(make([]byte, 0, 1024), 1024*1024)

	go func() {
		<-ctx.Done()
		_ = tty.Close()
	}()

	for reader.Scan() {
		line := strings.TrimSpace(reader.Text())
		if line == "" {
			continue
		}

		currentArgs, _ := c.snapshotConnectionState()
		command, err := parseInteractiveCommand(currentArgs, line, c.clientLogger())
		if err != nil {
			if writeErr := writeControlLine(tty, "interactive query error: "+err.Error()); writeErr != nil {
				return writeErr
			}
			continue
		}

		switch command.kind {
		case "help":
			if helpErr := c.writeInteractiveHelp(tty); helpErr != nil {
				return helpErr
			}
		case "show":
			if stateErr := c.writeInteractiveState(tty); stateErr != nil {
				return stateErr
			}
		case "quit":
			if quitErr := writeControlLine(tty, "quitting interactive session"); quitErr != nil {
				return quitErr
			}
			cancel()
			return nil
		case "reload":
			if reloadErr := c.applyInteractiveReload(command.next, command.spec); reloadErr != nil {
				if writeErr := writeControlLine(tty, "reload failed: "+reloadErr.Error()); writeErr != nil {
					return writeErr
				}
				continue
			}
			if successWriteErr := writeControlLine(tty, "reload applied successfully"); successWriteErr != nil {
				return successWriteErr
			}
		default:
			if unsupportedErr := writeControlLine(tty, "unsupported command"); unsupportedErr != nil {
				return unsupportedErr
			}
		}
	}

	if err := reader.Err(); err != nil && ctx.Err() == nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}

func (c *baseClient) applyInteractiveReload(nextArgs config.Args, nextSpec SessionSpec) error {
	prevArgs, prevSpec, connections := c.snapshotMutableState()
	if len(connections) == 0 {
		return errors.New("no active connections")
	}

	var unsupported []string
	for _, conn := range connections {
		if !conn.SupportsQueryUpdates(interactiveControlTimeout) {
			unsupported = append(unsupported, conn.Server())
		}
	}
	if len(unsupported) > 0 {
		return fmt.Errorf("%w: %s", connectors.ErrSessionUnsupported, strings.Join(unsupported, ", "))
	}

	applied, generation, err := c.applyInteractiveReloadConnections(connections, nextSpec)
	if err != nil {
		return c.rollbackInteractiveReload(applied, prevArgs, prevSpec, err)
	}

	if c.profile.commit != nil {
		if commitErr := c.profile.commit(nextSpec, generation); commitErr != nil {
			return c.rollbackInteractiveReload(applied, prevArgs, prevSpec,
				fmt.Errorf("commit session state: %w", commitErr))
		}
	}

	c.storeReloadState(nextArgs, nextSpec)
	return nil
}

func (c *baseClient) applyInteractiveReloadConnections(connections []connectors.Connector, nextSpec SessionSpec) ([]interactiveReloadState, uint64, error) {
	var generation uint64
	applied := make([]interactiveReloadState, 0, len(connections))
	for _, conn := range connections {
		prevSpec, prevGeneration, _ := conn.CommittedSession()
		if err := conn.ApplySessionSpec(nextSpec, interactiveControlTimeout); err != nil {
			if shouldRollbackFailedReload(err) {
				rollbackGeneration := generation
				if rollbackGeneration == 0 {
					rollbackGeneration = prevGeneration
				}
				applied = append(applied, interactiveReloadState{
					conn:               conn,
					spec:               prevSpec,
					rollbackGeneration: rollbackGeneration,
				})
			}
			return applied, 0, fmt.Errorf("%s: %w", conn.Server(), err)
		}
		applied = append(applied, interactiveReloadState{
			conn: conn,
			spec: prevSpec,
		})

		_, committedGeneration, ok := conn.CommittedSession()
		if !ok || committedGeneration == 0 {
			return applied, 0, fmt.Errorf("%s: missing committed session generation", conn.Server())
		}
		applied[len(applied)-1].rollbackGeneration = committedGeneration
		if generation == 0 {
			generation = committedGeneration
			continue
		}
		if generation != committedGeneration {
			return applied, 0, fmt.Errorf("mismatched committed generations: got %d and %d", generation, committedGeneration)
		}
	}
	return applied, generation, nil
}

func shouldRollbackFailedReload(err error) bool {
	return errors.Is(err, connectors.ErrSessionAckTimeout) ||
		errors.Is(err, connectors.ErrUnexpectedSessionAck)
}

func (c *baseClient) rollbackInteractiveReload(applied []interactiveReloadState, prevArgs config.Args, prevSpec SessionSpec, err error) error {
	rollbackErr := c.rollbackInteractiveReloadConnections(applied)
	c.storeReloadState(prevArgs, prevSpec)
	if rollbackErr != nil {
		return errors.Join(err, rollbackErr)
	}
	return err
}

func (*baseClient) rollbackInteractiveReloadConnections(applied []interactiveReloadState) error {
	var rollbackErr error
	for i := len(applied) - 1; i >= 0; i-- {
		if err := applied[i].conn.ApplySessionSpecWithGeneration(applied[i].spec, applied[i].rollbackGeneration, interactiveControlTimeout); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("%s: rollback session spec: %w", applied[i].conn.Server(), err))
		}
	}
	return rollbackErr
}

func (c *baseClient) writeInteractiveHelp(writer io.Writer) error {
	return writeControlLine(writer,
		"Commands: :reload <flags>, :show, :help, :quit. Use quotes around multi-word values such as --query \"select count(status) from stats\".")
}

func (c *baseClient) writeInteractiveState(writer io.Writer) error {
	args, spec, connections := c.snapshotMutableState()
	ready := 0
	for _, conn := range connections {
		if conn.SupportsQueryUpdates(0) {
			ready++
		}
	}

	line := fmt.Sprintf(
		"mode=%s files=%s query=%q regex=%q options=%q timeout=%d capable=%d/%d",
		args.Mode,
		strings.Join(spec.Files, ","),
		spec.Query,
		spec.Regex,
		spec.Options,
		spec.Timeout,
		ready,
		len(connections),
	)
	return writeControlLine(writer, line)
}

func parseInteractiveCommand(current config.Args, line string, logger logging.Logger) (interactiveCommand, error) {
	line = strings.TrimSpace(line)

	switch {
	case line == ":help":
		return interactiveCommand{kind: "help"}, nil
	case line == ":show":
		return interactiveCommand{kind: "show"}, nil
	case line == ":quit":
		return interactiveCommand{kind: "quit"}, nil
	case strings.HasPrefix(line, ":reload"):
		remainder := strings.TrimSpace(strings.TrimPrefix(line, ":reload"))
		if remainder == "" {
			return interactiveCommand{}, errors.New("reload requires flags to change")
		}
		tokens, err := splitInteractiveArgs(remainder)
		if err != nil {
			return interactiveCommand{}, err
		}
		nextArgs, err := parseInteractiveReloadArgs(current, tokens)
		if err != nil {
			return interactiveCommand{}, err
		}
		nextSpec, err := buildInteractiveSessionSpec(nextArgs, logger)
		if err != nil {
			return interactiveCommand{}, err
		}
		return interactiveCommand{
			kind: "reload",
			next: nextArgs,
			spec: nextSpec,
		}, nil
	default:
		return interactiveCommand{}, fmt.Errorf("unknown command %q", line)
	}
}

func parseInteractiveReloadArgs(current config.Args, tokens []string) (config.Args, error) {
	next := current
	fs := flag.NewFlagSet("reload", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.StringVar(&next.What, "files", current.What, "File(s) to read")
	fs.BoolVar(&next.Plain, "plain", current.Plain, "Plain output mode")
	fs.BoolVar(&next.Quiet, "quiet", current.Quiet, "Quiet output mode")
	fs.IntVar(&next.Timeout, "timeout", current.Timeout, "Max time dtail server will collect data until disconnection")

	switch {
	case isInteractiveQueryMode(current):
		fs.StringVar(&next.QueryStr, "query", current.QueryStr, "Map reduce query")
	case current.Mode == omode.GrepClient || current.Mode == omode.TailClient:
		var grep string
		fs.StringVar(&next.RegexStr, "regex", current.RegexStr, "Regular expression")
		fs.StringVar(&grep, "grep", "", "Alias for -regex")
		fs.BoolVar(&next.RegexInvert, "invert", current.RegexInvert, "Invert regex")
		fs.IntVar(&next.BeforeContext, "before", current.BeforeContext, "Leading context lines")
		fs.IntVar(&next.AfterContext, "after", current.AfterContext, "Trailing context lines")
		fs.IntVar(&next.MaxCount, "max", current.MaxCount, "Maximum number of matches")
		if err := fs.Parse(tokens); err != nil {
			return current, err
		}
		if grep != "" {
			next.RegexStr = grep
		}
		if len(fs.Args()) > 0 {
			return current, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
		}
		return next, nil
	case current.Mode == omode.CatClient:
	default:
		return current, fmt.Errorf("interactive reload is unsupported for mode %s", current.Mode)
	}

	if err := fs.Parse(tokens); err != nil {
		return current, err
	}
	if len(fs.Args()) > 0 {
		return current, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	return next, nil
}

func buildInteractiveSessionSpec(args config.Args, logger logging.Logger) (SessionSpec, error) {
	normalizedArgs, err := normalizeInteractiveArgs(args, logger)
	if err != nil {
		return SessionSpec{}, err
	}

	spec := NewSessionSpec(normalizedArgs)
	if _, commandErr := spec.Commands(); commandErr != nil {
		return SessionSpec{}, commandErr
	}
	return spec, nil
}

func normalizeInteractiveArgs(args config.Args, logger logging.Logger) (config.Args, error) {
	if !isInteractiveQueryMode(args) {
		return args, nil
	}

	_, regexValue, err := maprRegexFromQueryString(args.QueryStr, logger)
	if err != nil {
		return args, err
	}
	args.RegexStr = regexValue
	return args, nil
}

func isInteractiveQueryMode(args config.Args) bool {
	return strings.TrimSpace(args.QueryStr) != "" &&
		(args.Mode == omode.MapClient || args.Mode == omode.TailClient)
}

func splitInteractiveArgs(raw string) ([]string, error) {
	var (
		tokens  []string
		current strings.Builder
		inQuote rune
		escaped bool
	)

	flush := func() {
		if current.Len() == 0 {
			return
		}
		tokens = append(tokens, current.String())
		current.Reset()
	}

	for _, r := range raw {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case inQuote != 0:
			if r == inQuote {
				inQuote = 0
				continue
			}
			current.WriteRune(r)
		case r == '\'' || r == '"':
			inQuote = r
		case r == ' ' || r == '\t':
			flush()
		default:
			current.WriteRune(r)
		}
	}

	if escaped {
		return nil, errors.New("unterminated escape sequence")
	}
	if inQuote != 0 {
		return nil, errors.New("unterminated quoted string")
	}

	flush()
	return tokens, nil
}

func writeControlLine(writer io.Writer, message string) error {
	_, err := fmt.Fprintf(writer, "%s\n", message)
	return err
}

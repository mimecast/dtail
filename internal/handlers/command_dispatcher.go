package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/lcontext"
)

const maxCommandTimeoutSeconds = 24 * 60 * 60

type handleCommandCb func(context.Context, lcontext.LContext, int, []string, string)
type prepareCommandContextCb func(context.Context, string) (context.Context, func())

type commandCancelKeyType struct{}
type sessionCommandAdmissionKeyType struct{}
type commandAdmissionResultKeyType struct{}

type commandAdmissionResult struct {
	admitted bool
}

type commandDispatcher struct {
	handler                 *baseHandler
	handleCommandCb         handleCommandCb
	prepareCommandContextCb prepareCommandContextCb
	codec                   protocolCodec
	activeCommands          int32
	commandMu               sync.Mutex
	commandInitWg           sync.WaitGroup
	commandWg               sync.WaitGroup
	stopping                bool
	aborting                bool

	optionsOnce sync.Once
	optionsMu   sync.Mutex
	quiet       bool
	plain       bool
	serverless  bool
}

var commandCancelKey commandCancelKeyType
var sessionCommandAdmissionKey sessionCommandAdmissionKeyType
var commandAdmissionResultKey commandAdmissionResultKeyType

func withSessionCommandAdmission(ctx context.Context) context.Context {
	return context.WithValue(ctx, sessionCommandAdmissionKey, true)
}

func hasSessionCommandAdmission(ctx context.Context) bool {
	admitted, _ := ctx.Value(sessionCommandAdmissionKey).(bool)
	return admitted
}

func markCommandAdmitted(ctx context.Context) {
	if result, ok := ctx.Value(commandAdmissionResultKey).(*commandAdmissionResult); ok {
		result.admitted = true
	}
}

func withCommandCancel(ctx context.Context, cancel context.CancelFunc) context.Context {
	if cancel == nil {
		return ctx
	}
	return context.WithValue(ctx, commandCancelKey, cancel)
}

func cancelCommandContext(ctx context.Context) {
	cancel, ok := ctx.Value(commandCancelKey).(context.CancelFunc)
	if !ok || cancel == nil {
		return
	}
	cancel()
}

func applyCommandTimeout(ctx context.Context, args []string, argc int) (context.Context, []string, int, error) {
	if argc < 3 || args[0] != "timeout" {
		return ctx, args, argc, nil
	}

	seconds, err := strconv.Atoi(args[1])
	if err != nil {
		return ctx, args, argc, fmt.Errorf("invalid timeout value %q: %w", args[1], err)
	}
	if seconds > maxCommandTimeoutSeconds {
		return ctx, args, argc, fmt.Errorf("timeout value %d exceeds maximum of %d seconds",
			seconds, maxCommandTimeoutSeconds)
	}
	if seconds <= 0 {
		return ctx, args[2:], argc - 2, nil
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	parentCancel, _ := ctx.Value(commandCancelKey).(context.CancelFunc)
	combined := func() {
		cancel()
		if parentCancel != nil {
			parentCancel()
		}
	}

	return withCommandCancel(timeoutCtx, combined), args[2:], argc - 2, nil
}

func (d *commandDispatcher) handleCommand(command string) {
	h := d.handler
	h.Logger().Debug(h.user, commandForLog(command))

	args, argc, add, err := d.codec.handleProtocolVersion(strings.Split(command, " "))
	if err != nil {
		// No failed-command message here: a client of another protocol
		// version may not know hidden messages or their framing.
		h.send(h.serverMessages, h.Logger().Error(h.user, err)+add)
		return
	}
	args, argc, err = d.codec.handleBase64(args, argc)
	if err != nil {
		h.sendln(h.serverMessages, h.Logger().Error(h.user, err))
		h.reportCommandFailure(commandFailureProtocol)
		return
	}

	ctx, cancel := d.newCommandContext()
	ctx = withCommandCancel(ctx, cancel)
	if dispatchErr := d.dispatchCommand(ctx, args, argc); dispatchErr != nil {
		cancel()
		h.sendln(h.serverMessages, h.Logger().Error(h.user, dispatchErr))
		h.reportCommandFailure(commandFailureRejected)
	}
}

func (d *commandDispatcher) dispatchCommand(ctx context.Context, args []string, argc int) error {
	ctx, args, argc, err := applyCommandTimeout(ctx, args, argc)
	if err != nil {
		return err
	}

	parts := strings.SplitN(args[0], ":", 2)
	commandName := parts[0]
	ltx := lcontext.LContext{}
	if len(parts) == 2 && len(parts[1]) != 0 {
		options, parsedContext, deserializeErr := config.DeserializeOptions([]string{parts[1]})
		if deserializeErr != nil {
			return deserializeErr
		}
		d.handleOptions(options)
		ltx = parsedContext
		ctx = d.withReadShareOption(ctx, options[config.ReadShareOption])
	}

	if d.prepareCommandContextCb != nil {
		var cleanup func()
		ctx, cleanup = d.prepareCommandContextCb(ctx, commandName)
		if cleanup != nil {
			defer cleanup()
		}
	}

	d.handleCommandCb(ctx, ltx, argc, args, commandName)
	return nil
}

func (d *commandDispatcher) handleProtocolVersion(args []string) ([]string, int, string, error) {
	return d.codec.handleProtocolVersion(args)
}

func (d *commandDispatcher) handleBase64(args []string, argc int) ([]string, int, error) {
	return d.codec.handleBase64(args, argc)
}

func (d *commandDispatcher) handleRawCommand(ctx context.Context, command string) error {
	args := strings.Fields(command)
	if len(args) == 0 {
		return fmt.Errorf("empty command")
	}
	return d.dispatchCommand(ctx, args, len(args))
}

func (d *commandDispatcher) newCommandContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(d.handler.commandRootCtx)
}

func (d *commandDispatcher) handleAckCommand(argc int, args []string) {
	h := d.handler
	if argc < 3 {
		if !d.quiet {
			h.sendln(h.serverMessages, h.Logger().Warn(h.user,
				"Unable to parse command", args, argc))
		}
		return
	}
	if args[1] == "close" && args[2] == "connection" {
		h.acknowledgeClose()
	}
}

func (d *commandDispatcher) handleOptions(options map[string]string) {
	h := d.handler
	d.optionsMu.Lock()
	defer d.optionsMu.Unlock()
	d.optionsOnce.Do(func() {
		if quiet := options["quiet"]; quiet == "true" {
			h.Logger().Debug(h.user, "Enabling quiet mode")
			d.quiet = true
		}
		if plain := options["plain"]; plain == "true" {
			h.Logger().Debug(h.user, "Enabling plain mode")
			d.plain = true
		}
		// The "serverless" option is deliberately not honoured here. It
		// decides whether the session payload is written to this process'
		// own stdout instead of the session transport, which only the
		// in-process serverless runtime may do. It is derived once from the
		// runtime (Dependencies.ServerlessOutput) when the handler is built,
		// so a remote client cannot divert its payload into the dserver log.
		if options["serverless"] == "true" && !d.serverless {
			h.Logger().Debug(h.user, "Ignoring serverless option of a non-serverless session")
		}
	})
}

func (d *commandDispatcher) beginCommand(admittedSessionWork bool) bool {
	d.commandMu.Lock()
	defer d.commandMu.Unlock()
	if d.aborting || (d.stopping && !admittedSessionWork) {
		return false
	}
	d.commandWg.Add(1)
	d.commandInitWg.Add(1)
	atomic.AddInt32(&d.activeCommands, 1)
	return true
}

func (d *commandDispatcher) finishCommandInitialization() {
	d.commandInitWg.Done()
}

func (d *commandDispatcher) decrementActiveCommands() int32 {
	atomic.AddInt32(&d.activeCommands, -1)
	return atomic.LoadInt32(&d.activeCommands)
}

func (d *commandDispatcher) finishCommand() {
	d.commandWg.Done()
}

func (d *commandDispatcher) isStopping() bool {
	d.commandMu.Lock()
	defer d.commandMu.Unlock()
	return d.stopping
}

func (d *commandDispatcher) stopCommandAdmission() {
	d.commandMu.Lock()
	d.stopping = true
	d.commandMu.Unlock()
}

func (d *commandDispatcher) cancelCommandWork() {
	d.commandMu.Lock()
	d.handler.cancelCommands()
	d.commandMu.Unlock()
}

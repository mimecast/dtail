package handlers

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/lcontext"
)

// TestApplyCommandTimeout exercises the pure prefix-stripping helper that
// restores server-side handling of the "timeout N <cmd>..." command emitted by
// the client when --timeout>0 (see internal/session/spec.go). The server-side
// parser was removed in 2020 while the client kept emitting the prefix, so the
// command reached the dispatcher as an unknown command "timeout". Each case
// checks the returned args/argc and whether a context deadline was applied.
func TestApplyCommandTimeout(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantArgs     []string
		wantErr      bool
		wantDeadline bool // expect a context deadline on the returned ctx
	}{
		{
			name:     "no timeout prefix passes through unchanged",
			args:     []string{"cat", "file", "regex:noop"},
			wantArgs: []string{"cat", "file", "regex:noop"},
		},
		{
			name:         "positive timeout strips prefix and sets deadline",
			args:         []string{"timeout", "5", "cat", "file", "regex:noop"},
			wantArgs:     []string{"cat", "file", "regex:noop"},
			wantDeadline: true,
		},
		{
			name:         "tail read command timeout strips prefix",
			args:         []string{"timeout", "30", "tail", "file", "regex:noop"},
			wantArgs:     []string{"tail", "file", "regex:noop"},
			wantDeadline: true,
		},
		{
			name:     "zero timeout strips prefix without a deadline",
			args:     []string{"timeout", "0", "cat", "file"},
			wantArgs: []string{"cat", "file"},
		},
		{
			name:     "negative timeout strips prefix without a deadline",
			args:     []string{"timeout", "-5", "cat", "file"},
			wantArgs: []string{"cat", "file"},
		},
		{
			name:    "non-numeric timeout is rejected",
			args:    []string{"timeout", "abc", "cat", "file"},
			wantErr: true,
		},
		{
			// A huge N would overflow time.Duration(seconds)*time.Second into a
			// negative (already-elapsed) deadline; the max-seconds guard rejects
			// it instead of cancelling the read immediately.
			name:    "out-of-range timeout is rejected (overflow guard)",
			args:    []string{"timeout", "9223372036854775807", "cat", "file"},
			wantErr: true,
		},
		{
			name:     "bare timeout with too few args is not treated as a prefix",
			args:     []string{"timeout", "5"},
			wantArgs: []string{"timeout", "5"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, gotArgs, gotArgc, err := applyCommandTimeout(context.Background(), tc.args, len(tc.args))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (args=%v)", gotArgs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
				t.Fatalf("args = %v, want %v", gotArgs, tc.wantArgs)
			}
			if gotArgc != len(tc.wantArgs) {
				t.Fatalf("argc = %d, want %d", gotArgc, len(tc.wantArgs))
			}
			_, hasDeadline := ctx.Deadline()
			if hasDeadline != tc.wantDeadline {
				t.Fatalf("ctx deadline present = %v, want %v", hasDeadline, tc.wantDeadline)
			}
		})
	}
}

// TestDispatchCommandStripsTimeoutPrefix is the end-to-end regression guard for
// the reported bug: a "timeout N cat ..." command must dispatch the underlying
// "cat" command (with a deadline) instead of hitting the unknown-command path.
// Pre-fix, dispatchCommand split "timeout" as the command name and the read
// never ran ("Received unknown user command"), which hung dmap/dtail --timeout.
func TestDispatchCommandStripsTimeoutPrefix(t *testing.T) {

	handler := newSessionTestHandler("timeout-prefix-user")
	readServerMessage(t, handler.serverMessages)

	type captured struct {
		name string
		args []string
		ctx  context.Context
	}
	ch := make(chan captured, 1)
	record := func(commandName string) commandHandler {
		return func(ctx context.Context, _ lcontext.LContext, _ int, args []string, commandFinished func()) {
			ch <- captured{name: commandName, args: args, ctx: ctx}
			commandFinished()
		}
	}
	handler.commands = map[string]commandHandler{"cat": record("cat")}
	handler.handleCommandCb = func(ctx context.Context, ltx lcontext.LContext, argc int, args []string, commandName string) {
		if command, found := handler.commands[commandName]; found {
			command(ctx, ltx, argc, args, func() {})
			return
		}
		t.Errorf("unexpected command name %q (args=%v) — timeout prefix not stripped", commandName, args)
	}

	if err := handler.handleRawCommand(context.Background(), "timeout 7 cat test.log regex:noop"); err != nil {
		t.Fatalf("handleRawCommand returned error: %v", err)
	}

	select {
	case got := <-ch:
		if got.name != "cat" {
			t.Fatalf("dispatched command = %q, want cat", got.name)
		}
		wantArgs := []string{"cat", "test.log", "regex:noop"}
		if !reflect.DeepEqual(got.args, wantArgs) {
			t.Fatalf("args = %v, want %v", got.args, wantArgs)
		}
		if _, ok := got.ctx.Deadline(); !ok {
			t.Fatal("expected a context deadline from the timeout prefix")
		}
	case <-time.After(time.Second):
		t.Fatal("cat command was not dispatched; timeout prefix likely unhandled")
	}
}

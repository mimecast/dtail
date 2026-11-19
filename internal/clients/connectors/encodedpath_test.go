package connectors

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	sessionspec "github.com/mimecast/dtail/internal/session"
)

const spacedTestPath = "/var/log/my app/a.log"

// TestDispatchInitialCommandsSendsSpacedPathWithoutCapability checks both
// bootstraps, the plain command stream and SESSION START: every protocol 5
// server decodes encoded read paths, so a path containing a space or
// starting with base64% is sent, encoded, to a server advertising no
// capability at all.
func TestDispatchInitialCommandsSendsSpacedPathWithoutCapability(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		handler := &mockHandler{
			waitForCapabilities: true,
			sessionAcks:         []handlers.SessionAck{{Action: "start", Generation: 1}},
		}
		spec := sessionspec.Spec{Mode: omode.TailClient,
			Files: []string{spacedTestPath, protocol.EncodedReadPathPrefix + "a.log"}}
		commands, err := spec.Commands()
		if err != nil {
			t.Fatal(err)
		}

		if err := dispatchInitialCommands(context.Background(), "srv1", handler, commands, interactive,
			spec, &committedSessionState{}, logging.NopLogger{}); err != nil {
			t.Fatalf("interactive=%v: dispatchInitialCommands() error = %v", interactive, err)
		}
		want := []string{protocol.InputBatchBeginCommand, commands[0], commands[1],
			protocol.InputBatchCompleteCommand}
		if interactive {
			start, startErr := spec.StartCommand()
			if startErr != nil {
				t.Fatal(startErr)
			}
			want = []string{start}
		}
		if strings.Join(handler.commands, "\n") != strings.Join(want, "\n") {
			t.Fatalf("interactive=%v: sent commands %#v, want %#v", interactive, handler.commands, want)
		}
		if handler.Status() != 0 || handler.serverError != "" {
			t.Fatalf("interactive=%v: reported %q, status %d", interactive, handler.serverError, handler.Status())
		}
	}
}

// TestCheckJournalCapability checks the up-front check of a :reload: it
// returns ErrJournalUnsupported for a journal target the server cannot read
// and reports nothing; other targets, spaced paths included, need no
// capability.
func TestCheckJournalCapability(t *testing.T) {
	tests := []struct {
		name         string
		files        []string
		capabilities map[string]bool
		wantErr      error
	}{
		{name: "plain path", files: []string{"/var/log/app.log"}},
		{name: "spaced path", files: []string{spacedTestPath}},
		{name: "journal supported", files: []string{"journal:ssh.service"},
			capabilities: map[string]bool{protocol.CapabilityJournalV1: true}},
		{name: "journal unsupported", files: []string{"journal:ssh.service", spacedTestPath},
			wantErr: ErrJournalUnsupported},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockHandler{waitForCapabilities: true, capabilities: tc.capabilities}
			conn := &ServerConnection{server: "srv1", handler: mock}
			spec := sessionspec.Spec{Mode: omode.TailClient, Files: tc.files}
			if err := CheckJournalCapability(conn, spec, 10*time.Millisecond); !errors.Is(err, tc.wantErr) {
				t.Fatalf("CheckJournalCapability() error = %v, want %v", err, tc.wantErr)
			}
			if mock.Status() != 0 || mock.serverError != "" {
				t.Fatalf("check reported %q and set status %d", mock.serverError, mock.Status())
			}
		})
	}
}

// TestShouldReportConnectionError checks that missing journal support,
// which requireJournalCapability already reported, is not
// reported a second time when it ends the connection, also when wrapped,
// while other errors are, unless they are the canceled context's own.
func TestShouldReportConnectionError(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{name: "wrapped journal unsupported", ctx: context.Background(),
			err: fmt.Errorf("start session: %w", ErrJournalUnsupported)},
		{name: "plain error", ctx: context.Background(),
			err: errors.New("connection refused"), want: true},
		{name: "canceled context error", ctx: canceled,
			err: fmt.Errorf("dial: %w", context.Canceled)},
		{name: "other error after cancel", ctx: canceled,
			err: errors.New("connection refused"), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldReportConnectionError(tt.ctx, tt.err); got != tt.want {
				t.Fatalf("shouldReportConnectionError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

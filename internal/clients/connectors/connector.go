package connectors

import (
	"context"
	"time"

	"github.com/mimecast/dtail/internal/clients/handlers"
	sessionspec "github.com/mimecast/dtail/internal/session"
)

// Connector interface.
type Connector interface {
	// Start the connection.
	Start(ctx context.Context, cancel context.CancelFunc, throttleCh, statsCh chan struct{})
	// Server hostname.
	Server() string
	// Handler for the connection.
	Handler() handlers.Handler
	// ApplySessionSpec starts or updates the interactive session workload on an
	// already connected server (SESSION START/UPDATE, which every protocol 5
	// server supports).
	ApplySessionSpec(spec sessionspec.Spec, timeout time.Duration) error
	// ApplySessionSpecWithGeneration starts or updates the interactive session
	// workload using the provided committed generation as the base for the
	// session update command.
	ApplySessionSpecWithGeneration(spec sessionspec.Spec, generation uint64, timeout time.Duration) error
	// CommittedSession returns the last session spec and generation that the
	// server acknowledged for this connection.
	CommittedSession() (sessionspec.Spec, uint64, bool)
	// ReplaceStartSessionSpec makes spec the workload of the connection's
	// initial SESSION START if that has not begun yet (the connection is
	// still connecting) and reports true then; false once it began, after
	// which only a committed session can be updated. An interactive :reload
	// calls it for every connection without an active session, so one that
	// comes up later does not start the replaced workload. A SESSION START
	// in flight is waited for, but for at most sessionUpdateAckMaxWait; it
	// returns ErrSessionStartPending then.
	ReplaceStartSessionSpec(spec sessionspec.Spec) (bool, error)
	// RestoreStartSessionSpec is ReplaceStartSessionSpec for the rollback
	// of a failed :reload. A SESSION START in flight that is not
	// acknowledged within sessionUpdateAckMaxWait updates its session to
	// spec right after it is: it returns ErrSessionRollbackDeferred then.
	RestoreStartSessionSpec(spec sessionspec.Spec) (bool, error)
}

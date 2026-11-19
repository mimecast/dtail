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
	// SupportsQueryUpdates reports whether the connected server advertised
	// runtime query replacement support within the given timeout.
	SupportsQueryUpdates(timeout time.Duration) bool
	// ApplySessionSpec starts or updates the interactive session workload on an
	// already connected server when query updates are supported.
	ApplySessionSpec(spec sessionspec.Spec, timeout time.Duration) error
	// ApplySessionSpecWithGeneration starts or updates the interactive session
	// workload using the provided committed generation as the base for the
	// session update command.
	ApplySessionSpecWithGeneration(spec sessionspec.Spec, generation uint64, timeout time.Duration) error
	// CommittedSession returns the last session spec and generation that the
	// server acknowledged for this connection.
	CommittedSession() (sessionspec.Spec, uint64, bool)
	// RestoreCommittedSession resets the local committed session snapshot without
	// advancing the generation.
	RestoreCommittedSession(spec sessionspec.Spec, generation uint64, committed bool)
}

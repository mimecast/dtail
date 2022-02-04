package connectors

import (
	"context"

	"github.com/mimecast/dtail/internal/clients/handlers"
)

// Connector interface.
type Connector interface {
	// Start the connection.
	Start(ctx context.Context, cancel context.CancelFunc, throttleCh, statsCh chan struct{})
	// Server hostname.
	Server() string
	// Handler for the connection.
	Handler() handlers.Handler
}

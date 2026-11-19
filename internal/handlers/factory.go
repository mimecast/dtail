package handlers

import (
	"context"
	"fmt"
	"io"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/fs/readhub"
	"github.com/mimecast/dtail/internal/io/journal"
	"github.com/mimecast/dtail/internal/logging"
	user "github.com/mimecast/dtail/internal/sessionuser"
)

// Dependencies contains the process-owned resources used by a session handler.
type Dependencies struct {
	ServerConfig *config.ServerConfig
	CatLimiter   chan struct{}
	TailLimiter  chan struct{}
	// ReadHub shares follow reads of the same file between sessions; nil
	// makes every session read privately (see NewReadHub).
	ReadHub      *readhub.Hub
	AuthKeyStore *authkey.Store
	// ServerlessOutput is set by the in-process client runtime only. A
	// non-nil writer puts the handler into serverless mode, where payload
	// goes to this process' own output instead of the session transport.
	// dserver leaves it nil, so a remote session can never select that mode.
	ServerlessOutput io.Writer
	Loggers          HandlerLoggers
	Capabilities     []string
	// Colorizer paints the serverless payload lines (unless plain). The
	// in-process client runtime decides about terminal colors and passes a
	// pass-through colorizer when they are off; nil means the default colors.
	Colorizer Colorizer
	// JournalUnits caches the journal unit list of the path listing for
	// all sessions of a dserver (NewJournalUnitCache); nil makes every
	// journal: listing run journalctl itself.
	JournalUnits *journal.UnitCache
	Hostname     string
}

// NewForUser creates the handler appropriate for the authenticated user.
func NewForUser(ctx context.Context, user *user.User, dependencies Dependencies) (Handler, error) {
	if ctx == nil {
		return nil, fmt.Errorf("create handler: context must not be nil")
	}
	if user != nil && user.Name == config.HealthUser {
		maxFrameSize := config.DefaultMaxCommandFrameSize
		if dependencies.ServerConfig != nil && dependencies.ServerConfig.MaxCommandFrameSize > 0 {
			maxFrameSize = dependencies.ServerConfig.MaxCommandFrameSize
		}
		return NewHealthHandler(ctx, user, maxFrameSize, dependencies.Hostname,
			dependencies.Loggers.Diagnostics)
	}
	return NewServerHandler(ctx, user, dependencies)
}

// NewReadHub returns the hub that lets dserver sessions tailing the same file
// share one reader of it, and the scheduled jobs of one group share one-shot
// reads of the same file, configured like a session's private reader, or nil
// when the configuration turns shared reads off.
func NewReadHub(serverCfg *config.ServerConfig, logger logging.Logger) *readhub.Hub {
	if serverCfg == nil || serverCfg.SharedReadsDisable {
		return nil
	}
	timings := newReadTimings(serverCfg)
	return readhub.New(readhub.Options{
		Logger:        logger,
		MaxLineLength: timings.maxLineLength,
		RetryInterval: timings.readRetryInterval,
		// Every member reading in a one-shot group read holds one of the
		// server's cat slots during the read; members without a free slot
		// when the read starts read on their own.
		MaxGroupMembers: max(1, serverCfg.MaxConcurrentCats),
	})
}

// NewJournalUnitCache returns the cache of the journal units for the path
// listings of all dserver sessions: journalctl --field _SYSTEMD_UNIT runs
// at most once per journal.DefaultUnitCacheTTL, and concurrent listings
// share one run. Each listing still filters the units by its own session
// user's read permissions.
func NewJournalUnitCache() *journal.UnitCache {
	return journal.NewUnitCache(journal.ListUnits, journal.DefaultUnitCacheTTL)
}

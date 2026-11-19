package clients

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/protocol"
)

const (
	// probeMaxConnections is how many connections a probe round asks at
	// most: the fields of a log are usually the same on every server, so a
	// few answers suffice and large server lists cause no probe storm.
	probeMaxConnections = 3
	// probeInterval is the time between two probe rounds while nothing else
	// starts one; a round is only started by it while the output is not
	// paused.
	probeInterval = 60 * time.Second
	// probeRoundTimeout bounds one probe round: waiting for a connection's
	// session to come up, sending the request and waiting for the answer
	// (the server stops sampling after 10 s).
	probeRoundTimeout = 30 * time.Second
	// probeStdin is the file name of stdin in serverless mode, which a
	// probe never reads.
	probeStdin = "-"
)

// interactiveProber probes the servers of an interactive session in the
// background (the probe command, docs/dev/probe.md) and stores their answers
// in its fieldCatalog, for the query completion. It sends the request with
// the session's current files to at most probeMaxConnections connections:
// once the session started, after every applied :reload (request), on
// demand (:probe, probeNow, which waits for the round's result), and every
// probeInterval while the output is not paused.
//
// Tab cannot wait for a server: while the output is paused (for typing or by
// an empty line) the paused writers block the connection handler that reads
// the server's frames, so with server connections no answer can arrive until
// the output runs again (TestProbeAnswerWhileOutputPaused; in serverless
// mode the payload bypasses the handler and answers of a running probe do
// arrive, but the in-process server logs to the paused output while it
// handles a new request). The prober therefore runs ahead of time and the
// query completion only reads the catalog. The path completion of --files
// also reads listings the requested rounds prefetched (run); for another
// directory, or once a listing expired, it asks on demand (listPaths,
// interactive_probe_paths.go) and waits briefly with the output running
// (waitForListing).
//
// The prober never blocks the session: it runs on a goroutine of its own,
// one round at a time, and a request while a round runs starts one more
// round after it. Rounds read the connections afresh, so a connection
// replaced by a reconnect is probed in the next round. It sends a probe
// only, which changes neither the session's workload nor its exit status,
// and logs at debug level only, so it prints nothing. It stops when the
// context passed to start ends (with the session).
type interactiveProber struct {
	catalog *fieldCatalog
	// connections and files return the session's current connections and
	// files (the client's state, which reconnects and :reload change).
	connections func() []connectors.Connector
	files       func() []string
	// serverless is set for a serverless session (paths are local, "-"
	// may read stdin); stdinPiped tells whether stdin is no terminal
	// (stdinIsPipe, replaced by tests). Both decide readsNoFiles.
	serverless bool
	stdinPiped func() bool
	logger     clientlog.Logger
	now        func() time.Time

	interval     time.Duration
	roundTimeout time.Duration

	trigger      chan struct{}
	outputPaused atomic.Bool
	// refreshListings is set by :probe (probeNow): the next requested
	// round lists the files' directories also when their listing is
	// cached fresh, so that a file created since shows up.
	refreshListings atomic.Bool
	sequence        atomic.Uint64
	done            chan struct{}
	// rounds counts the rounds and keeps the last one's result, for
	// :probe (probeNow).
	rounds probeRounds

	// listWait is how long a Tab waits for a path listing it needs
	// (pathListingWait); the listing state is in interactive_probe_paths.go.
	listWait time.Duration
	listing  pathListingState
}

// newInteractiveProber returns the prober of c's interactive session; it
// does nothing before start.
func (c *baseClient) newInteractiveProber() *interactiveProber {
	return &interactiveProber{
		catalog:     newFieldCatalog(),
		connections: c.snapshotConnections,
		files: func() []string {
			_, spec := c.snapshotConnectionState()
			return spec.Files
		},
		serverless:   c.Serverless,
		stdinPiped:   stdinIsPipe,
		logger:       c.clientLogger(),
		now:          time.Now,
		interval:     probeInterval,
		roundTimeout: probeRoundTimeout,
		trigger:      make(chan struct{}, 1),
		done:         make(chan struct{}),
		listWait:     pathListingWait,
		listing:      pathListingState{pending: make(map[string]chan struct{})},
	}
}

// start runs the prober until ctx ends, with a first round right away (its
// probes wait for each session to come up).
func (p *interactiveProber) start(ctx context.Context) {
	p.startListings(ctx)
	p.request()
	go func() {
		defer close(p.done)
		p.run(ctx)
	}()
}

// wait returns once the prober started by start stopped, its path
// listings included.
func (p *interactiveProber) wait() {
	<-p.done
	p.waitListings()
}

// catalogSnapshot returns the field catalog merged over the servers'
// answers about the session's current files (not those read before a
// :reload), which Tab completion of a --query value and :fields read;
// empty for a nil prober (no interactive session). NoFilesHint is set for
// a session that reads no files (readsNoFiles).
func (p *interactiveProber) catalogSnapshot() catalogSnapshot {
	if p == nil {
		return catalogSnapshot{}
	}
	snap := p.catalog.snapshot(probeFiles(p.files()))
	snap.NoFilesHint = p.noFilesHint(true)
	return snap
}

// request asks for a probe round as soon as possible; it never blocks. A
// nil prober (no interactive session) does nothing.
func (p *interactiveProber) request() {
	if p == nil {
		return
	}
	select {
	case p.trigger <- struct{}{}:
	default:
		// A round is requested already; it reads the current state.
	}
}

// setOutputPaused tells the prober whether the output is paused, which
// holds back the periodic rounds. A nil prober does nothing.
func (p *interactiveProber) setOutputPaused(paused bool) {
	if p != nil {
		p.outputPaused.Store(paused)
	}
}

// run runs the probe rounds: requested ones, and periodic ones every
// interval after the previous round while the output is not paused. Each
// requested round (session start, :reload, :probe) is followed by the
// prefetch of the path listings (prefetchListings; fresh cached listings
// are only asked for again after :probe); a periodic one is not:
// a listing makes the server check the permissions of every file of the
// directory, which every minute of every session cost dserver CPU time,
// and Tab fetches an expired listing on demand anyway.
func (p *interactiveProber) run(ctx context.Context) {
	timer := time.NewTimer(p.interval)
	defer timer.Stop()
	for {
		requested := false
		select {
		case <-ctx.Done():
			return
		case <-p.trigger:
			requested = true
		case <-timer.C:
			if p.outputPaused.Load() {
				timer.Reset(p.interval)
				continue
			}
		}
		p.rounds.begin()
		p.rounds.end(p.round(ctx))
		if requested {
			// The path listings of the files' directories are prefetched in
			// the background once the round counted as finished, so :probe
			// reports the field probes without waiting for them (up to
			// roundTimeout more), and Tab completes the siblings of the
			// files read from the cache. After :probe the cached
			// listings are refreshed too (refreshListings).
			p.prefetchListings(p.refreshListings.Swap(false))
		}
		timer.Reset(p.interval)
	}
}

// round probes the session's current files on up to probeMaxConnections of
// its current connections, concurrently, and returns when every probe
// answered or failed, or roundTimeout passed. It returns what came of each
// probe, for :probe.
func (p *interactiveProber) round(ctx context.Context) probeRoundResult {
	files := probeFiles(p.files())
	if len(files) == 0 {
		return probeRoundResult{}
	}
	targets := probeTargets(p.connections())
	if len(targets) == 0 {
		return probeRoundResult{files: len(files)}
	}
	roundCtx, cancel := context.WithTimeout(ctx, p.roundTimeout)
	defer cancel()

	outcomes := make([]probeOutcome, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		request := protocol.ProbeRequest{ID: p.nextID(), Files: files}
		wg.Go(func() { outcomes[i] = p.probe(roundCtx, target, request) })
	}
	wg.Wait()
	return newProbeRoundResult(len(files), targets, outcomes)
}

// probe sends request to one connection, stores the answer and returns
// what came of it.
func (p *interactiveProber) probe(ctx context.Context, target probeTarget,
	request protocol.ProbeRequest) probeOutcome {

	answer, err := target.prober.Probe(ctx, request)
	if err != nil {
		p.logger.Debug(target.server, "Probe failed", request.ID, err)
		return probeFailed
	}
	if isBusyProbeAnswer(answer) {
		// The catalog ignores it too (fieldCatalog.store).
		p.logger.Debug(target.server, "Probe answered busy", request.ID)
		return probeBusy
	}
	p.catalog.store(target.server, request.Files, answer, p.now())
	p.logger.Debug(target.server, "Probe answer stored in the field catalog", request.ID,
		"formats", len(answer.Formats), "filesSampled", answer.FilesSampled,
		"linesSampled", answer.LinesSampled, "errors", len(answer.Errors))
	return probeAnswered
}

// nextID returns a new probe id ("p1", "p2", ...), unique per prober and so
// per connection.
func (p *interactiveProber) nextID() string {
	return "p" + strconv.FormatUint(p.sequence.Add(1), 10)
}

// probeTarget is a connection a probe round asks.
type probeTarget struct {
	server string
	prober handlers.Prober
}

// probeTargets returns the connections a probe round asks, at most
// probeMaxConnections, in this order: first those whose session is up (the
// server's capabilities frame arrived), then those still connecting, each
// in the client's order. A connection whose handler cannot probe, or whose
// session ended, is never asked: it waits for its reconnect, which replaces
// it, or never had a session (a connection whose Start ended without one
// has its handler shut down, ServerConnection.Start). Those still
// connecting only fill the slots the sessions up leave; such a probe waits
// for its session to come up, at most the round's timeout, or until its
// dial fails. So a server first in --servers that is down neither holds up
// a round (and :probe) nor takes the path listings, which go to the first
// target (list): the first connection up, or the first one still
// connecting while none is up (the round at the session's start). A
// reconnect starts no round of its own: the reconnected connection is
// asked by the next round, a periodic one within probeInterval at the
// latest (while the output is not paused).
func probeTargets(connections []connectors.Connector) []probeTarget {
	var up, connecting []probeTarget
	for _, conn := range connections {
		if len(up) == probeMaxConnections {
			break
		}
		handler := conn.Handler()
		prober, ok := handler.(handlers.Prober)
		if !ok || handlerDone(handler) {
			continue
		}
		target := probeTarget{server: conn.Server(), prober: prober}
		if handler.WaitForCapabilities(0) {
			up = append(up, target)
		} else if len(connecting) < probeMaxConnections {
			connecting = append(connecting, target)
		}
	}
	up = append(up, connecting...)
	return up[:min(len(up), probeMaxConnections)]
}

// handlerDone reports whether handler's session ended.
func handlerDone(handler handlers.Handler) bool {
	select {
	case <-handler.Done():
		return true
	default:
		return false
	}
}

// probeFiles returns the request entries of the session's files: each
// written as a read command's path word (protocol.EncodeReadPath), stdin
// left out, at most protocol.ProbeMaxFiles.
func probeFiles(files []string) []string {
	entries := make([]string, 0, min(len(files), protocol.ProbeMaxFiles))
	for _, file := range files {
		if len(entries) == protocol.ProbeMaxFiles {
			break
		}
		if file == "" || file == probeStdin {
			continue
		}
		entries = append(entries, protocol.EncodeReadPath(file))
	}
	return entries
}

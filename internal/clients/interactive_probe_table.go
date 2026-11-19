package clients

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mimecast/dtail/internal/protocol"
)

// tableProbeWait is how long :fields TABLE waits, with the output running,
// for a table probe of TABLE it asked for (awaitTableProbe). The server
// stops sampling after 10 s, but a table probe normally answers within
// milliseconds.
const tableProbeWait = 2 * time.Second

// requestTable asks the probe targets (probeTargets, at most
// probeMaxConnections) for a table-targeted probe of table over the
// session's files (protocol.ProbeRequest.Table): the server samples a
// larger window for the lines of that table only, so that a table logging
// rarely gets fields and $variables in the catalog. It returns a channel
// closed once every target answered (the answers are in the catalog,
// storeTable) or failed, or the request gave up after roundTimeout; the
// request goes on when the caller stops waiting. started reports whether
// this call sent the request: a table probe on its way is not sent again,
// its channel is returned with started false, so that a caller can wait
// only once per request (completeEditorLine). It returns nil, sending
// nothing, when the catalog holds a fresh answer about table and the
// current files or a table probe of them failed within
// tableProbeRetryAfter (hasTableProbe), when the session reads no files but
// stdin, for a table no server accepts (protocol.ValidProbeTable), and for
// a nil prober or one not started or stopped. It never blocks on a server.
//
// The pending requests are looked at before the catalog, both under the
// listing lock: a request ends by storing its answers and only then leaving
// the pending ones (tableProbeDone), so a call sees it either pending or
// stored and never sends it twice.
func (p *interactiveProber) requestTable(table string) (done <-chan struct{}, started bool) {
	if p == nil || !protocol.ValidProbeTable(table) {
		return nil, false
	}
	files := probeFiles(p.files())
	if len(files) == 0 {
		return nil, false
	}
	p.listing.mu.Lock()
	defer p.listing.mu.Unlock()
	ctx := p.listing.ctx
	if ctx == nil || ctx.Err() != nil {
		return nil, false
	}
	if pending, ok := p.listing.tables[table]; ok {
		return pending, false
	}
	if p.catalog.hasTableProbe(files, table, p.now()) {
		return nil, false
	}
	return p.startTableProbe(ctx, table, files), true
}

// startTableProbe sends the table probe of table over files in the
// background and records it as pending. The caller holds the listing lock.
func (p *interactiveProber) startTableProbe(ctx context.Context, table string, files []string) <-chan struct{} {
	if p.listing.tables == nil {
		p.listing.tables = make(map[string]chan struct{})
	}
	done := make(chan struct{})
	p.listing.tables[table] = done
	p.listing.sequence++
	id := "t" + strconv.FormatUint(p.listing.sequence, 10)
	p.listing.wg.Go(func() {
		defer p.tableProbeDone(table, done)
		p.probeTable(ctx, protocol.ProbeRequest{ID: id, Files: files, Table: table})
	})
	return done
}

// awaitTableProbe has :fields TABLE show what a table probe found: it asks
// for a table probe of the table (named as the catalog spells it,
// catalogTableName) and waits for it at most tableProbeWait with the output
// running (interactivePause.runOutput), as a server's answer is only read
// while the output runs (see interactiveProber). It returns at once when a
// fresh answer is cached or nothing can be probed (requestTable).
func (c *baseClient) awaitTableProbe(pause *interactivePause, table string) {
	done, _ := c.prober.requestTable(catalogTableName(c.prober.catalogSnapshot(), table))
	if done == nil {
		return
	}
	pause.runOutput(func() {
		timer := time.NewTimer(tableProbeWait)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		}
	})
}

// catalogTableName returns the table of snap matching typed ignoring case
// (as :fields matches tables), or typed in upper case, as a query's from
// clause names a table, when snap has none.
func catalogTableName(snap catalogSnapshot, typed string) string {
	for _, table := range catalogTables(snap) {
		if strings.EqualFold(table, typed) {
			return table
		}
	}
	return strings.ToUpper(typed)
}

// tableProbeDone forgets the pending table probe and closes its channel.
func (p *interactiveProber) tableProbeDone(table string, done chan struct{}) {
	p.listing.mu.Lock()
	delete(p.listing.tables, table)
	p.listing.mu.Unlock()
	close(done)
}

// probeTable sends request to the probe targets concurrently and stores
// their answers. Failures are logged at debug level only. When no target
// stored an answer (every probe failed, e.g. a server that does not answer
// while it floods follow output, or answered busy), the failure is recorded
// in the catalog (markTableProbeFailed), so that the table is not asked for
// again on every Tab, only after tableProbeRetryAfter.
func (p *interactiveProber) probeTable(ctx context.Context, request protocol.ProbeRequest) {
	roundCtx, cancel := context.WithTimeout(ctx, p.roundTimeout)
	defer cancel()
	var wg sync.WaitGroup
	var stored atomic.Int32
	for _, target := range probeTargets(p.connections()) {
		wg.Go(func() {
			answer, err := target.prober.Probe(roundCtx, request)
			if err != nil {
				p.logger.Debug(target.server, "Table probe failed", request.ID, request.Table, err)
				return
			}
			if p.catalog.storeTable(target.server, request.Files, request.Table, answer, p.now()) {
				stored.Add(1)
			}
			p.logger.Debug(target.server, "Table probe answered", request.ID, request.Table,
				"formats", len(answer.Formats), "linesSampled", answer.LinesSampled,
				"errors", len(answer.Errors), "busy", isBusyProbeAnswer(answer))
		})
	}
	wg.Wait()
	if stored.Load() == 0 && ctx.Err() == nil {
		p.catalog.markTableProbeFailed(request.Files, request.Table, p.now())
	}
}

package clients

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/protocol"
)

// pathListingWait is how long a Tab waits for a path listing that is not
// cached: the line editor must never block for long. With a remote server
// the output runs meanwhile, as the answer is only read while it runs (see
// interactiveProber).
const pathListingWait = time.Second

// pathListingState is the prober's state of its on-demand requests: the
// path listings for the --files completion and the table-targeted probes
// (requestTable, interactive_probe_table.go) requested and not yet
// answered, each sent once however often it is asked for, and the context
// they run in. Both run in the background, are bounded by roundTimeout and
// are waited for by waitListings.
type pathListingState struct {
	mu      sync.Mutex
	ctx     context.Context
	pending map[string]chan struct{}
	// tables are the pending table probes by table name.
	tables map[string]chan struct{}
	wg     sync.WaitGroup
	// sequence numbers the listing ids ("l1", "l2", ...) and the table
	// probe ids ("t1", "t2", ...), apart from the field probes' ids.
	sequence uint64
}

// startListings lets listPaths send listings until ctx ends.
func (p *interactiveProber) startListings(ctx context.Context) {
	p.listing.mu.Lock()
	defer p.listing.mu.Unlock()
	p.listing.ctx = ctx
}

// waitListings returns once every listing started by listPaths ended. It
// first stops listPaths from starting more (under the lock that every
// start holds), so no listing is added while it waits.
func (p *interactiveProber) waitListings() {
	p.listing.mu.Lock()
	p.listing.ctx = nil
	p.listing.mu.Unlock()
	p.listing.wg.Wait()
}

// listPaths asks the first target of probeTargets (the first connection up)
// for the path listing prefix (protocol.ProbeRequest.List) and returns a
// channel closed once the answer is in the catalog, or the request failed
// or gave up after roundTimeout. A request for the same prefix still on its
// way is not sent again: its channel is returned. The request does not end
// with the caller's wait, so an answer that comes late is still cached and
// the next Tab uses it. A nil prober, or one not started or stopped,
// returns nil.
func (p *interactiveProber) listPaths(prefix string) <-chan struct{} {
	if p == nil {
		return nil
	}
	p.listing.mu.Lock()
	defer p.listing.mu.Unlock()
	ctx := p.listing.ctx
	if ctx == nil || ctx.Err() != nil {
		return nil
	}
	if done, ok := p.listing.pending[prefix]; ok {
		return done
	}
	done := make(chan struct{})
	p.listing.pending[prefix] = done
	p.listing.sequence++
	id := "l" + strconv.FormatUint(p.listing.sequence, 10)
	p.listing.wg.Go(func() {
		defer p.listingDone(prefix, done)
		p.list(ctx, protocol.ProbeRequest{ID: id, List: prefix})
	})
	return done
}

// listingDone forgets the pending listing prefix and closes its channel.
func (p *interactiveProber) listingDone(prefix string, done chan struct{}) {
	p.listing.mu.Lock()
	delete(p.listing.pending, prefix)
	p.listing.mu.Unlock()
	close(done)
}

// list sends the listing request to the first target of probeTargets (the
// first connection up) and caches the answer. Failures are logged at debug
// level only.
func (p *interactiveProber) list(ctx context.Context, request protocol.ProbeRequest) {
	targets := probeTargets(p.connections())
	if len(targets) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, p.roundTimeout)
	defer cancel()
	target := targets[0]
	answer, err := target.prober.Probe(ctx, request)
	if err != nil {
		p.logger.Debug(target.server, "Path listing failed", request.ID, err)
		return
	}
	p.catalog.storeListing(target.server, request.List, answer, p.now())
	p.logger.Debug(target.server, "Path listing stored", request.ID,
		"entries", len(answer.Entries), "truncated", answer.Truncated, "errors", len(answer.Errors))
}

// pathListingSource returns what the --files completion reads: the cached
// listings at the current time and the number of connections. A nil
// prober has no listings and asks for none.
func (p *interactiveProber) pathListingSource() pathCompletionSource {
	if p == nil {
		return pathCompletionSource{find: func(string, string) listingMatch {
			return listingMatch{complete: true}
		}}
	}
	return pathCompletionSource{
		find: func(dir, base string) listingMatch {
			return p.catalog.findListing(dir, base, p.now())
		},
		servers: len(p.connections()),
	}
}

// prefetchListings asks for the listings of the directories of the
// session's files, so that Tab in a --files value completes the files next
// to the ones being read (rotated logs) without waiting for a server. It
// does not wait for them: they run in the background like any listPaths
// request (each bounded by roundTimeout, waited for by wait), so neither
// the next round nor :probe waits for them. The handler sends them one at
// a time with the field probes (its probe slot). Unless refresh is set (on
// :probe), a directory whose whole listing is cached and not older than
// pathListingMaxAge is not asked for again (e.g. on a :reload of the same
// files).
func (p *interactiveProber) prefetchListings(refresh bool) {
	now := p.now()
	for _, prefix := range prefetchPrefixes(p.files()) {
		if !refresh && p.catalog.findListing(prefix, "", now).complete {
			continue
		}
		p.listPaths(prefix)
	}
}

// prefetchPrefixes returns the listing requests of the directories of
// files, each once, at most protocol.ProbeMaxFiles: the directory key of
// every entry (listingDirKey), leaving out stdin, directories with glob
// characters, which name no one directory, and journal targets: listing
// the journal units runs journalctl --field _SYSTEMD_UNIT on the server
// (seconds of CPU on a large journal), so they are listed only when Tab
// asks for them.
func prefetchPrefixes(files []string) []string {
	var prefixes []string
	for _, file := range files {
		if len(prefixes) == protocol.ProbeMaxFiles {
			break
		}
		if file == "" || file == probeStdin {
			continue
		}
		dir, _ := listingDirKey(file)
		if dir == fs.JournalSpecPrefix || strings.ContainsAny(dir, "*?[") {
			continue
		}
		if !slices.Contains(prefixes, dir) {
			prefixes = append(prefixes, dir)
		}
	}
	return prefixes
}

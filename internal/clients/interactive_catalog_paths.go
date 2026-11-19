package clients

import (
	"slices"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/protocol"
)

const (
	// pathListingMaxAge is how long a cached path listing is used: a
	// directory gains files (rotated logs), so an older listing is asked
	// for again. The requested rounds (session start, :reload, :probe)
	// prefetch the directories of the session's files when their listing
	// is older; the periodic rounds do not, so an expired listing is
	// fetched by the next Tab that needs it.
	pathListingMaxAge = 2 * time.Minute
	// pathListingTransientMaxAge is how long a listing that failed for the
	// moment (transientListingError) is used: long enough for a Tab right
	// after it to show the reason (an 'unable to list' answer usually
	// arrives within the Tab's wait; a 'timed out' one only after the
	// server's probe timeout, so only a Tab within the next seconds shows
	// it), short enough that a later Tab asks again instead of showing the
	// error for pathListingMaxAge.
	pathListingTransientMaxAge = 5 * time.Second
	// pathListingMaxCached bounds the cached path listings; the oldest goes
	// first.
	pathListingMaxCached = 64
	// pathListingWorkingDir is the directory key of a path without a
	// directory part: the server's working directory, which a listing
	// names "./" (a listing of "" would be invalid).
	pathListingWorkingDir = "./"
)

// pathListing is a cached answer to a path listing request (the probe
// command with protocol.ProbeRequest.List, docs/dev/probe.md "Path
// listing"), as one server (the first connection up) gave it.
type pathListing struct {
	server string
	// prefix is the request's List: a directory key (see findListing)
	// followed by the start of the names it lists.
	prefix    string
	entries   []protocol.ProbeEntry
	truncated bool
	// err is the first error reason of the answer (e.g. listing disabled,
	// no journal), "" without one.
	err string
	// transient is set when the listing failed for the moment
	// (transientListingError): it is used for pathListingTransientMaxAge
	// only, and, like a truncated one, never for a narrower prefix.
	transient bool
	received  time.Time
}

// maxAge returns how long the listing is used after it arrived.
func (l pathListing) maxAge() time.Duration {
	if l.transient {
		return pathListingTransientMaxAge
	}
	return pathListingMaxAge
}

// listingMatch is what the cache holds for completing the names starting
// with base in a directory (findListing).
type listingMatch struct {
	// listing is the best cached listing covering base, valid when found.
	listing pathListing
	found   bool
	// complete is set when listing holds every permitted name starting
	// with base, or no narrower request could add one; otherwise fetch is
	// the listing request to send.
	complete bool
	fetch    string
}

// storeListing caches answer, the server's answer to the path listing
// request prefix, received at the given time. An answer that only says
// that another probe of the session was running is not cached: it says
// nothing about the directory. An answer that failed for the moment (the
// probe timed out, journalctl failed) is cached as transient and truncated:
// the server sets truncated on a scan timeout, but not when the journal
// unit list times out or fails, and such an answer must not complete a
// narrower prefix either.
func (c *fieldCatalog) storeListing(server, prefix string, answer protocol.ProbeAnswer, received time.Time) {
	if isBusyProbeAnswer(answer) {
		return
	}
	listing := pathListing{server: server, prefix: prefix, entries: answer.Entries,
		truncated: answer.Truncated, received: received}
	if len(answer.Errors) > 0 {
		listing.err = answer.Errors[0].Error
	}
	if slices.ContainsFunc(answer.Errors, transientListingError) {
		listing.transient, listing.truncated = true, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listings[prefix] = listing
	for len(c.listings) > pathListingMaxCached {
		c.evictOldestListing()
	}
}

// transientListingError reports whether a listing answer's error means
// that the listing failed for the moment, so that asking again may give
// the names: the probe timed out (a slow file system or journal) or the
// journal units could not be listed.
func transientListingError(probeErr protocol.ProbeError) bool {
	return probeErr.Error == protocol.ProbeErrorTimeout || probeErr.Error == protocol.ProbeErrorListFailed
}

// evictOldestListing drops the listing received first; c.mu is held.
func (c *fieldCatalog) evictOldestListing() {
	oldest := ""
	for prefix, listing := range c.listings {
		if oldest == "" || listing.received.Before(c.listings[oldest].received) {
			oldest = prefix
		}
	}
	delete(c.listings, oldest)
}

// findListing returns what the cache holds for the names starting with
// base in the directory dir, a directory key: a directory path with its
// trailing "/" as typed, pathListingWorkingDir, or fs.JournalSpecPrefix for
// the journal units. A cached listing of prefix dir+q covers base when q is
// a prefix of base and, as names starting with "." are only listed for a q
// starting with ".", q is not "" for a base starting with ".". Listings
// older than their maxAge (at now: pathListingMaxAge, or
// pathListingTransientMaxAge for a transient one) do not count. The best one is the
// complete (not truncated) listing with the longest q; failing that, a
// truncated one, which is complete only when q is base itself (a narrower
// request would answer the same). The request to fetch is then dir+base,
// narrowing a truncated listing, or, with nothing cached, the whole
// directory (dir, or dir+"." for hidden names), which later Tabs of the
// same directory can use as well.
func (c *fieldCatalog) findListing(dir, base string, now time.Time) listingMatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	var match listingMatch
	for prefix, listing := range c.listings {
		q, ok := strings.CutPrefix(prefix, dir)
		if !ok || strings.Contains(q, "/") || !listingCovers(q, base) ||
			now.Sub(listing.received) > listing.maxAge() {
			continue
		}
		if !match.found || betterListing(listing, match.listing) {
			match.listing, match.found = listing, true
		}
	}
	if match.found {
		match.complete = !match.listing.truncated || match.listing.prefix == dir+base
		if !match.complete {
			match.fetch = dir + base
		}
		return match
	}
	match.fetch = dir
	if strings.HasPrefix(base, ".") {
		match.fetch += "."
	}
	return match
}

// listingCovers reports whether a listing of the names starting with q
// holds every name starting with base.
func listingCovers(q, base string) bool {
	return strings.HasPrefix(base, q) && (q != "" || !strings.HasPrefix(base, "."))
}

// betterListing reports whether a is a better listing than b for the same
// base: complete before truncated, then the narrower one (longer prefix).
func betterListing(a, b pathListing) bool {
	if a.truncated != b.truncated {
		return !a.truncated
	}
	return len(a.prefix) > len(b.prefix)
}

// listingDirKey splits a --files entry into the directory key of its
// listing (see findListing) and the start of the names to complete: a
// journal target lists the units, a path the directory up to its last
// "/" (the working directory without one).
func listingDirKey(entry string) (dir, base string) {
	if rest, ok := strings.CutPrefix(entry, fs.JournalSpecPrefix); ok {
		return fs.JournalSpecPrefix, rest
	}
	i := strings.LastIndexByte(entry, '/')
	if i < 0 {
		return pathListingWorkingDir, entry
	}
	return entry[:i+1], entry[i+1:]
}

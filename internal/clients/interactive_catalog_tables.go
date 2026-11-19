package clients

import (
	"cmp"
	"maps"
	"slices"
	"time"

	"github.com/mimecast/dtail/internal/protocol"
)

const (
	// tableProbeMaxAge is how long the answer of a table-targeted probe
	// counts as fresh: a table typed again within it is not probed again
	// (interactiveProber.requestTable). A plain probe round runs as often.
	tableProbeMaxAge = probeInterval
	// catalogMaxTableProbes bounds the table probe answers the catalog
	// keeps (per server and table, over all files), and so its failed
	// table probes (per table); the oldest is dropped first.
	catalogMaxTableProbes = 64
	// tableProbeRetryAfter is how long a table probe no server answered
	// (failed, timed out or busy) keeps the table from being asked for
	// again, as a transient listing error does: a server that does not
	// answer (e.g. while it floods follow output) is not asked on every Tab.
	tableProbeRetryAfter = 5 * time.Second
)

// catalogTableKey names the table probe answer of one server.
type catalogTableKey struct {
	server, table string
}

// tableProbeFailure is a table probe of files no server answered, at the
// given time.
type tableProbeFailure struct {
	files []string
	at    time.Time
}

// storeTable records answer as server's latest answer to a table-targeted
// probe of table over files, received at the given time, and reports
// whether it did. Like store it ignores a busy answer. An answer without
// protocol.ProbeAnswer.Table (a server that does not know table probes
// answered a plain probe) is kept for the table all the same: it is valid
// data about the files, and the table is not asked for again while it is
// fresh. At most catalogMaxTableProbes answers are kept.
func (c *fieldCatalog) storeTable(server string, files []string, table string,
	answer protocol.ProbeAnswer, received time.Time) bool {

	if isBusyProbeAnswer(answer) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := catalogTableKey{server: server, table: table}
	if _, ok := c.tableAnswers[key]; !ok && len(c.tableAnswers) >= catalogMaxTableProbes {
		c.dropOldestTableAnswer()
	}
	c.tableAnswers[key] = catalogAnswer{answer: answer, files: slices.Sorted(slices.Values(files)),
		received: received}
	return true
}

// markTableProbeFailed records that no server answered the table probe of
// table over files at the given time (see tableProbeRetryAfter), replacing
// an earlier failure of the table. At most catalogMaxTableProbes tables
// are kept, the oldest failure dropped first.
func (c *fieldCatalog) markTableProbeFailed(files []string, table string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.tableFailures[table]; !ok && len(c.tableFailures) >= catalogMaxTableProbes {
		var oldest string
		for name, failure := range c.tableFailures {
			if oldest == "" || failure.at.Before(c.tableFailures[oldest].at) {
				oldest = name
			}
		}
		delete(c.tableFailures, oldest)
	}
	c.tableFailures[table] = tableProbeFailure{files: slices.Sorted(slices.Values(files)), at: at}
}

// hasTableProbe reports whether table is not to be probed over files (in
// any order) now: a server answered a table probe of it within
// tableProbeMaxAge, or one no server answered failed within
// tableProbeRetryAfter.
func (c *fieldCatalog) hasTableProbe(files []string, table string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	wanted := slices.Sorted(slices.Values(files))
	for key, entry := range c.tableAnswers {
		if key.table == table && slices.Equal(entry.files, wanted) && now.Sub(entry.received) <= tableProbeMaxAge {
			return true
		}
	}
	failure, ok := c.tableFailures[table]
	return ok && slices.Equal(failure.files, wanted) && now.Sub(failure.at) <= tableProbeRetryAfter
}

// mergeTableAnswers merges the table probe answers about the files wanted
// (sorted) into snap, in server and table order: their formats (tables,
// fields, per-table $variables; a table answer has no format-wide ones),
// time, truncation and servers, and records each table with its sampled
// lines in TableProbes. Their errors are left out: they repeat those of
// the plain probes of the same files. The caller holds c.mu.
func (c *fieldCatalog) mergeTableAnswers(snap *catalogSnapshot, wanted []string) {
	keys := slices.SortedFunc(maps.Keys(c.tableAnswers), func(a, b catalogTableKey) int {
		return cmp.Or(cmp.Compare(a.server, b.server), cmp.Compare(a.table, b.table))
	})
	for _, key := range keys {
		entry := c.tableAnswers[key]
		if !slices.Equal(entry.files, wanted) {
			continue
		}
		snap.addAnswer(entry)
		if !slices.Contains(snap.Servers, key.server) {
			snap.Servers = append(snap.Servers, key.server)
		}
		if snap.TableProbes == nil {
			snap.TableProbes = make(map[string]int)
		}
		snap.TableProbes[key.table] += entry.answer.LinesSampled
	}
	slices.Sort(snap.Servers)
}

// dropOldestTableAnswer drops the table probe answer received first. The
// caller holds c.mu.
func (c *fieldCatalog) dropOldestTableAnswer() {
	var oldest catalogTableKey
	var oldestTime time.Time
	first := true
	for key, entry := range c.tableAnswers {
		if first || entry.received.Before(oldestTime) {
			oldest, oldestTime, first = key, entry.received, false
		}
	}
	delete(c.tableAnswers, oldest)
}

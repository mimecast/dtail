package client

import (
	"bytes"
	"fmt"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/protocol"
)

const (
	// maxPendingBytes is the size of the queued messages from which Merge
	// waits for the global group set's lock instead of queueing more, so a
	// connection whose merges keep missing the lock (e.g. while the result
	// of a huge group set is rendered) does not queue without bound.
	maxPendingBytes = 4 << 20
	// maxRetainedPendingBytes and maxRetainedPendingMessages bound the queue
	// storage kept for reuse after a merge; a larger queue (a merge that
	// missed the lock many times) is released.
	maxRetainedPendingBytes    = 1 << 20
	maxRetainedPendingMessages = 16 << 10
)

var (
	aggregateDelimiter   = []byte(protocol.AggregateDelimiter)
	aggregateKVDelimiter = []byte(protocol.AggregateKVDelimiter)
)

// Aggregate mapreduce data on the DTail client side.
//
// The aggregate messages of one server are queued (Add) and aggregated into
// the shared global group set in the order they arrived (Merge, Flush), each
// message directly into its group's set: the client adds a server's values to
// the global set like a merge of a set holding only that message would (see
// aggregateFields). Merging the messages of a whole receive buffer at once
// takes the global lock once per buffer instead of once per message, and no
// per-message group set, aggregate set or field map is allocated. As every
// message is aggregated on its own, in order, the result does not depend on
// how the messages were batched.
type Aggregate struct {
	// mu protects the complete local aggregation transaction: generation
	// validation, queueing, merge, and reset. Add and Merge run on the
	// connection's stdout-copy goroutine while Flush may run concurrently
	// during shutdown.
	mu sync.Mutex
	// pending holds the group keys and fields of the queued messages, which
	// messages index.
	pending  []byte
	messages []pendingMessage
	// fields is scratch space for the fields of the message being aggregated.
	fields []field
	// plan caches per-query data of the message aggregation.
	plan aggregatePlan
	// Shared per-client session state.
	session *SessionState
	// The generation of the queued messages.
	generation uint64
	// The server we aggregate the data for (logging and debugging purposes only)
	server string
	logger logging.Logger
	// retrying is set while a retry of a merge that missed the global
	// group set's lock is scheduled (retryMerge).
	retrying bool
	// heldFor is the session switch (SessionState.switchPending) the queued
	// messages are held for, 0 when none are held (see SessionAcked).
	heldFor uint64
	// rolledBack is set once a second acknowledgement arrived within the
	// switch heldFor: what is held since is the previous workload's again.
	rolledBack bool
	// acks counts the session acknowledgements of the connection
	// (SessionAcked): the first is its initial SESSION START's.
	acks int
	// startAbandoned is set when the workload of the connection's initial
	// SESSION START is one the client rolled back while the start was
	// still unacknowledged (AbandonStartWorkload).
	startAbandoned bool
	// failures are the errors of fields that could not be aggregated,
	// kept (at most maxKeptFailures) until mu is released (withLock).
	failures []error
}

// maxKeptFailures bounds Aggregate.failures.
const maxKeptFailures = 16

// mergeRetryDelay is how long after a merge that missed the global group
// set's lock the queued messages are tried again.
const mergeRetryDelay = 10 * time.Millisecond

// pendingMessage is a queued aggregate message: its group key is
// pending[start:keyEnd] and its fields are pending[keyEnd:end].
type pendingMessage struct {
	start, keyEnd, end int
	samples            int
}

// field is a key/value pair of an aggregate message, borrowed from pending.
type field struct {
	key, value []byte
}

// aggregatePlan holds what aggregating a message needs of its query.
type aggregatePlan struct {
	query *mapr.Query
	// avgCountKeys holds mapr.AvgCountKey of every avg() storage key, by
	// select index, so it is not built for every message.
	avgCountKeys []string
}

// NewAggregate create new client aggregator.
func NewAggregate(server string, session *SessionState, logger logging.Logger) *Aggregate {
	generation := uint64(0)
	if session != nil {
		generation = session.Generation()
	}

	return &Aggregate{
		session:    session,
		generation: generation,
		server:     server,
		logger:     logging.OrNop(logger),
	}
}

// Expected reports whether the session expects aggregate frames (see
// SessionState.AggregatesExpected); false without a session.
func (a *Aggregate) Expected() bool {
	return a.session != nil && a.session.AggregatesExpected()
}

// Add validates one aggregate message (the content of an aggregate frame)
// and queues it for the next Merge or Flush. message is copied, so the caller
// may reuse it right away. A message of another generation than the queued
// ones drops those first: they belong to a query that is no longer current.
// Messages held for a session switch (SessionAcked) are kept, as they
// belong to the generation the switch commits.
func (a *Aggregate) Add(message []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.session == nil {
		return fmt.Errorf("missing client mapreduce session state")
	}
	if a.startAbandoned && a.acks < 2 {
		// The abandoned start workload's (AbandonStartWorkload).
		return nil
	}
	if generation := a.session.Generation(); generation != a.generation && a.heldFor == 0 {
		a.reset()
		a.generation = generation
	}

	groupKey, samplesField, fields, ok := splitMessage(message)
	if !ok {
		return fmt.Errorf("aggregate message without any real data")
	}
	samples, parseErr := strconv.Atoi(borrowedString(samplesField))
	if parseErr != nil {
		return fmt.Errorf("unable to parse sample count '%s': %w", samplesField, parseErr)
	}

	start := len(a.pending)
	a.pending = append(append(a.pending, groupKey...), fields...)
	a.messages = append(a.messages, pendingMessage{
		start: start, keyEnd: start + len(groupKey), end: len(a.pending), samples: samples,
	})
	return nil
}

// Merge aggregates the queued messages into the shared global group set
// when its lock is free right away, and otherwise keeps them queued, so a
// connection never waits for another one, and tries again shortly
// (retryMerge) and at the next Merge or Flush. Only once maxPendingBytes
// are queued does it wait for the lock.
func (a *Aggregate) Merge() error {
	a.merge(false)
	return nil
}

// Flush aggregates any queued messages into the shared global group set,
// waiting for its lock: during shutdown the last messages must not be lost.
// Messages held for a pending session switch stay queued also here: the
// end of the switch merges them (see SessionAcked).
func (a *Aggregate) Flush() error {
	a.merge(true)
	return nil
}

// SessionAcked is told by the connection's handler, in the order of the
// received frames, that the server acknowledged a SESSION START or UPDATE:
// the aggregate messages after it are those of the workload it
// acknowledged.
//
// It matters while the client switches the session (SessionState.Switching,
// an interactive reload): the servers are updated one after the other and
// each starts the new workload as soon as it acknowledged the update, so
// its first aggregates (for a small file its whole result) can arrive
// before the client committed the new query. They used to be merged into
// the replaced query's result, or dropped when no query ran yet, and were
// missing in the new one's. Now the messages queued before the
// acknowledgement are merged into the still current result, and those
// after it are held until the switch ended, then merged into the result
// current by then: the committed query's, or, after a failed switch, the
// previous one's, to which a second acknowledgement within the switch
// (the rollback's, dropping what was held for the abandoned workload)
// brought the server back.
//
// The end of the switch settles what is held itself, waiting for the
// global group set's lock (SessionState.Switching(false) calls endHold of
// every holder, one after the other): once it returned, the result holds
// it, also of a connection whose session ended during the switch (its
// shutdown Flush keeps held messages) and without a further message
// arriving (a dmap server sends its result once). After a failed switch,
// what a connection holds without a second acknowledgement is the
// abandoned workload's (its session ended, or its rollback failed) and is
// dropped instead (settleHold).
//
// What is held is not bounded by maxPendingBytes: waiting would block the
// handler that must read the acknowledgements, and dropping lost rows of
// a result the client keeps in memory anyway once merged. It is bounded by
// what a server sends during one reload.
//
// The acknowledgement of a SESSION START holds too: a connection that
// comes up during the reload starts with the reloaded spec (see
// connectors.Connector.ReplaceStartSessionSpec). One whose start began
// before the reload, with the previous spec, is updated by the reload
// right after: the update's acknowledgement is then the second within the
// switch and drops what the previous query sent in between, and a reload
// failing before it was updated drops it at its end (usually nothing, a
// server's first aggregates come an interval after its start); the two
// starts can not be told apart here.
//
// Outside a switch (the initial SESSION START, a start's deferred
// rollback) nothing is held; what a start that is rolled back later sends
// until then is dropped instead (AbandonStartWorkload).
//
// It runs on the goroutine reading the connection's frames and waits for
// the global group set's lock to merge what was queued before the
// acknowledgement, so a result being rendered delays that connection's
// frames for as long as the rendering takes. Nothing is logged with mu
// held, here or in endHold: a paused client logger would block the end of
// the reload behind it.
func (a *Aggregate) SessionAcked() {
	if a.session == nil {
		return
	}
	a.withLock(func() {
		a.acks++
		pending := a.session.holdFor(a)
		if pending != 0 && a.heldFor == pending {
			a.reset()
			a.rolledBack = true
			return
		}
		// Queued before the acknowledgement (also what an ended switch
		// still holds): the current generation's.
		a.settleHold()
		a.mergeLocked(true)
		a.heldFor, a.rolledBack = pending, false
	})
}

// AbandonStartWorkload tells the aggregate, with abandoned set, that the
// workload the connection's initial SESSION START asked for is no longer
// the client's: a :reload gave the connecting connection its spec, failed,
// and its rollback could not wait for the start's acknowledgement, so the
// start rolls its session back itself with a SESSION UPDATE right after it
// (connectors.ErrSessionRollbackDeferred). Both acknowledgements then
// arrive outside the reload's switch, where nothing is held, and the
// aggregates the server sent in between, those of the reloaded query, were
// merged into the result of the query the client kept (for a dmap server
// reading a small file, its whole result). They are dropped now: every
// message up to the connection's second acknowledgement, the rollback's,
// after which the server runs the kept workload again. If the rollback is
// not acknowledged the connection ends, so nothing of it is merged.
//
// The connection tells it before it defers the rollback, and takes it
// back (abandoned false) if the start finished in that moment: the session
// is then rolled back by an update within the switch, as any committed one
// (SessionAcked).
func (a *Aggregate) AbandonStartWorkload(abandoned bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.startAbandoned = abandoned
}

// withLock runs fn with mu held and logs the aggregation failures it
// left (aggregateFields) after releasing mu: a logger that blocks (the
// interactive client's while its output is paused) must not hold mu, for
// which the end of a reload waits (endHold).
func (a *Aggregate) withLock(fn func()) {
	a.withLockLogging(a.logger, fn)
}

// withLockLogging is withLock logging the failures to logger.
func (a *Aggregate) withLockLogging(logger logging.Logger, fn func()) {
	a.mu.Lock()
	fn()
	failures := a.failures
	a.failures = nil
	a.mu.Unlock()
	for _, err := range failures {
		logger.Error(err)
	}
}

// settleHold ends holding the queued messages if the switch they are held
// for ended, and reports false while it is pending. After a committed
// switch they belong to the generation that is current now, and so they
// do after a failed one if a second acknowledgement brought the server
// back to the previous workload (rolledBack); else they are the abandoned
// workload's and are dropped. The caller holds mu.
func (a *Aggregate) settleHold() bool {
	if a.heldFor == 0 {
		return true
	}
	pending, committed := a.session.switchOutcome(a.heldFor)
	if pending {
		return false
	}
	if !committed && !a.rolledBack {
		a.reset()
	}
	a.heldFor, a.rolledBack = 0, false
	a.generation = a.session.Generation()
	return true
}

// endHold is called by the session state when a switch ended: it settles
// what is held for it (settleHold) and merges what is queued into the
// result that is current now, waiting for the global group set's lock.
//
// It runs on the goroutine that ends the switch, the interactive control
// loop, which must not block in a paused client logger (it alone answers
// the host key question that pauses it). So the failures are logged to the
// session state's logger, which the client makes a non-blocking one, and
// not to the connection's.
func (a *Aggregate) endHold() {
	a.withLockLogging(a.session.logger, func() {
		if a.settleHold() {
			a.mergeLocked(true)
		}
	})
}

// merge aggregates the queued messages into the global group set of the
// current generation, waiting for its lock when block is set or too much is
// queued. Queued messages of an earlier generation are dropped.
func (a *Aggregate) merge(block bool) {
	a.withLock(func() { a.mergeLocked(block) })
}

// mergeLocked is merge with mu held. Messages held for a session switch
// that is still pending stay queued (see SessionAcked).
func (a *Aggregate) mergeLocked(block bool) {
	// Add queues nothing without a session.
	if len(a.messages) == 0 {
		return
	}
	if !a.settleHold() {
		return
	}
	snapshot := a.session.Snapshot()
	if snapshot.Query == nil || snapshot.GlobalGroup == nil {
		// No query is active: interactive dtail switched its session to a
		// plain read (SessionState.ClearQuery). There is no result to add
		// them to, so they are dropped, as aggregates of a replaced
		// generation are. (The first aggregates of a query being switched
		// to are held until it is committed, see SessionAcked.)
		a.reset()
		return
	}
	if snapshot.Generation != a.generation {
		a.reset()
		a.generation = snapshot.Generation
		return
	}

	// The first merge of the generation from this server tells the reporter
	// that its data arrived (SessionSnapshot.AggregatesArrived); a missed
	// UpdateNoblock merges nothing yet and so signals nothing either.
	apply := func(group *mapr.GroupSet) {
		a.aggregatePending(snapshot.Query, group)
		snapshot.arrival.mark(a.server)
	}
	if block || len(a.pending) >= maxPendingBytes {
		snapshot.GlobalGroup.Update(apply)
		return
	}
	if !snapshot.GlobalGroup.UpdateNoblock(apply) && !a.retrying {
		a.retrying = true
		go a.retryMerge()
	}
}

// retryMerge merges the messages a Merge left queued because another
// connection (or the result rendering) held the global group set's lock.
// Without it they waited for the connection's next aggregate messages: the
// servers send their results at the same moment, once per interval, so a
// connection that lost the lock had its rows reported one interval late
// (seen live as a server missing from a query's first table). It never
// waits for the lock either: a merge that misses again schedules the next
// retry. Messages of a replaced generation are dropped by merge.
func (a *Aggregate) retryMerge() {
	time.Sleep(mergeRetryDelay)
	a.mu.Lock()
	a.retrying = false
	a.mu.Unlock()
	a.merge(false)
}

// aggregatePending aggregates every queued message, in order, into group and
// empties the queue. It runs under the global group set's lock.
func (a *Aggregate) aggregatePending(query *mapr.Query, group *mapr.GroupSet) {
	a.preparePlan(query)
	for _, message := range a.messages {
		set := group.GetSetBytes(a.pending[message.start:message.keyEnd])
		a.fields = appendFields(a.fields[:0], a.pending[message.keyEnd:message.end])
		a.aggregateFields(set, message.samples)
	}
	a.reset()
}

// reset empties the queue, releasing storage beyond the retention bounds.
func (a *Aggregate) reset() {
	a.pending, a.messages = a.pending[:0], a.messages[:0]
	if cap(a.pending) > maxRetainedPendingBytes {
		a.pending = nil
	}
	if cap(a.messages) > maxRetainedPendingMessages {
		a.messages = nil
	}
	// The scratch fields borrow pending (also beyond their length, from
	// messages with more fields); drop them with it.
	clear(a.fields[:cap(a.fields)])
	a.fields = a.fields[:0]
}

// preparePlan caches the avg() count keys of query.
func (a *Aggregate) preparePlan(query *mapr.Query) {
	if a.plan.query == query {
		return
	}
	a.plan = aggregatePlan{query: query, avgCountKeys: make([]string, len(query.Select))}
	for i, sc := range query.Select {
		if sc.Operation == mapr.Avg {
			a.plan.avgCountKeys[i] = mapr.AvgCountKey(sc.FieldStorage)
		}
	}
}

// aggregateFields aggregates the selected fields of the message in a.fields
// into set, the global set of its group. A field whose value cannot be
// aggregated is skipped and its error kept for the log (failures); the message's samples are added when any
// field was aggregated. The server already aggregated each value, so it is
// added with clientAggregation, which adds it to set as merging a set that
// holds only this message would (mapr.AggregateSet.Merge): a server's count
// adds up, min()/max() compare, last()/len() replace. A field the message
// lacks leaves set as it is.
func (a *Aggregate) aggregateFields(set *mapr.AggregateSet, samples int) {
	var addedSamples bool
	for i, sc := range a.plan.query.Select {
		val, ok := a.fieldValue(sc.FieldStorage)
		if !ok {
			continue
		}
		var err error
		if sc.Operation == mapr.Avg {
			err = a.aggregateAvg(set, sc.FieldStorage, a.plan.avgCountKeys[i], val)
		} else {
			err = set.Aggregate(sc.FieldStorage, sc.Operation, val, true)
		}
		if err != nil {
			// Logged once mu is released (withLock).
			if len(a.failures) < maxKeptFailures {
				a.failures = append(a.failures, err)
			}
			continue
		}
		addedSamples = true
	}
	if addedSamples {
		set.Samples += samples
	}
}

// aggregateAvg adds a server's avg() sum of storage, and the number of values
// it holds, to set. dserver sends that number with every avg() sum, under
// countKey (mapr.AvgCountKey(storage)), as the sum alone cannot be averaged:
// the message's samples count lines with any selected field, not values of
// this one. A sum without its count, or with a count that is not a positive
// integer, is a malformed message and rejects the field, sum included.
func (a *Aggregate) aggregateAvg(set *mapr.AggregateSet, storage, countKey, sum string) error {
	raw, ok := a.fieldValue(countKey)
	if !ok {
		return fmt.Errorf("avg() value of '%s' without its value count", storage)
	}
	count, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("unable to parse value count of '%s': %w", storage, err)
	}
	if count <= 0 {
		return fmt.Errorf("invalid value count '%s' of '%s'", raw, storage)
	}
	return set.AggregateAvg(storage, sum, count)
}

// fieldValue returns the value of key in the message being aggregated. The
// value borrows pending: it is only valid until the queue is reset, which
// mapr.AggregateSet.Aggregate allows (it copies the strings it keeps). When a
// message carries a key more than once, the last one wins, as a map of the
// fields would have it. That matters for len(): a dserver sends its length
// and then its string under the same key, and the string must win.
func (a *Aggregate) fieldValue(key string) (string, bool) {
	for i := len(a.fields) - 1; i >= 0; i-- {
		if string(a.fields[i].key) == key {
			return borrowedString(a.fields[i].value), true
		}
	}
	return "", false
}

// splitMessage splits an aggregate message "groupKey∥samples∥fields" into
// its parts. ok is false for a message of fewer than four
// protocol.AggregateDelimiter separated parts: one without any field, not
// even an empty one after a trailing delimiter.
func splitMessage(message []byte) (groupKey, samples, fields []byte, ok bool) {
	groupKey, rest, ok := bytes.Cut(message, aggregateDelimiter)
	if !ok {
		return nil, nil, nil, false
	}
	samples, fields, ok = bytes.Cut(rest, aggregateDelimiter)
	if !ok || !bytes.Contains(fields, aggregateDelimiter) {
		return nil, nil, nil, false
	}
	return groupKey, samples, fields, true
}

// appendFields appends the key/value pairs of the
// protocol.AggregateDelimiter separated parts of fields to dst. A part
// without protocol.AggregateKVDelimiter, such as the empty one after the
// trailing delimiter, carries no field.
func appendFields(dst []field, fields []byte) []field {
	for len(fields) > 0 {
		part, rest, _ := bytes.Cut(fields, aggregateDelimiter)
		if key, value, ok := bytes.Cut(part, aggregateKVDelimiter); ok {
			dst = append(dst, field{key: key, value: value})
		}
		fields = rest
	}
	return dst
}

// borrowedString reinterprets b as a string without copying it. The caller
// must guarantee that b is neither modified nor recycled while the returned
// string is still reachable.
func borrowedString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}

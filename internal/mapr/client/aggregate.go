package client

import (
	"bytes"
	"fmt"
	"strconv"
	"sync"
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
}

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

// Add validates one aggregate message (the content of an aggregate frame)
// and queues it for the next Merge or Flush. message is copied, so the caller
// may reuse it right away. A message of another generation than the queued
// ones drops those first: they belong to a query that is no longer current.
func (a *Aggregate) Add(message []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.session == nil {
		return fmt.Errorf("missing client mapreduce session state")
	}
	if generation := a.session.Generation(); generation != a.generation {
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
// when its lock is free right away, and otherwise keeps them queued for the
// next Merge or Flush, so a connection never waits for another one. Only
// once maxPendingBytes are queued does it wait for the lock.
func (a *Aggregate) Merge() error {
	return a.merge(false)
}

// Flush aggregates any queued messages into the shared global group set,
// waiting for its lock: during shutdown the last messages must not be lost.
func (a *Aggregate) Flush() error {
	return a.merge(true)
}

// merge aggregates the queued messages into the global group set of the
// current generation, waiting for its lock when block is set or too much is
// queued. Queued messages of an earlier generation are dropped.
func (a *Aggregate) merge(block bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Add queues nothing without a session.
	if len(a.messages) == 0 {
		return nil
	}
	snapshot := a.session.Snapshot()
	if snapshot.Query == nil || snapshot.GlobalGroup == nil {
		a.reset()
		if block {
			return nil
		}
		return fmt.Errorf("missing client mapreduce query state")
	}
	if snapshot.Generation != a.generation {
		a.reset()
		a.generation = snapshot.Generation
		return nil
	}

	apply := func(group *mapr.GroupSet) { a.aggregatePending(snapshot.Query, group) }
	if block || len(a.pending) >= maxPendingBytes {
		snapshot.GlobalGroup.Update(apply)
		return nil
	}
	snapshot.GlobalGroup.UpdateNoblock(apply)
	return nil
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
// aggregated is logged and skipped; the message's samples are added when any
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
			err = a.aggregateAvg(set, sc.FieldStorage, a.plan.avgCountKeys[i], val, samples)
		} else {
			err = set.Aggregate(sc.FieldStorage, sc.Operation, val, true)
		}
		if err != nil {
			a.logger.Error(err)
			continue
		}
		addedSamples = true
	}
	if addedSamples {
		set.Samples += samples
	}
}

// aggregateAvg adds a server's avg() sum of storage, and the number of values
// it holds, to set. A current dserver sends that number under countKey
// (mapr.AvgCountKey(storage)). An older one sends none: its sample count (the
// lines of the group with any selected field) is then taken instead, so such a
// server's average is what it always was. A count that is not a positive
// integer rejects the field, sum included.
func (a *Aggregate) aggregateAvg(set *mapr.AggregateSet, storage, countKey, sum string, samples int) error {
	count := samples
	if raw, ok := a.fieldValue(countKey); ok {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("unable to parse value count of '%s': %w", storage, err)
		}
		if parsed <= 0 {
			return fmt.Errorf("invalid value count '%s' of '%s'", raw, storage)
		}
		count = parsed
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

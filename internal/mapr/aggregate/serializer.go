package aggregate

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
)

type aggregateOutput struct {
	messages chan<- string
}

// serializer owns the accumulated result sets and every operation that moves
// them to the output stream. Its permit serializes periodic, requested, and
// final flushes without coupling Abort to an in-flight send.
type serializer struct {
	logger logging.Logger
	query  *mapr.Query

	groupMu   sync.Mutex
	groupSets map[string]*mapr.AggregateSet
	permit    chan struct{}
	requests  chan struct{}
	ticker    atomic.Pointer[time.Ticker]
	output    atomic.Pointer[aggregateOutput]
}

func newSerializer(query *mapr.Query, logger logging.Logger) *serializer {
	permit := make(chan struct{}, 1)
	permit <- struct{}{}
	return &serializer{
		logger:    logger,
		query:     query,
		groupSets: make(map[string]*mapr.AggregateSet),
		permit:    permit,
		requests:  make(chan struct{}, 1),
	}
}

func (s *serializer) countGroups() int {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	return len(s.groupSets)
}

func (s *serializer) startTicker(interval time.Duration) {
	s.ticker.Store(time.NewTicker(interval))
}

func (s *serializer) stopTicker() {
	// Start publishes the ticker atomically before launching the serialization
	// loop. A nil load means Start has not created it yet.
	if ticker := s.ticker.Load(); ticker != nil {
		ticker.Stop()
	}
}

func (s *serializer) prepareOutput(messages chan<- string) {
	if messages == nil {
		s.output.Store(nil)
		return
	}
	s.output.Store(&aggregateOutput{messages: messages})
}

func (s *serializer) request(ctx context.Context, done <-chan struct{}) {
	select {
	case <-ctx.Done():
		return
	case <-done:
		return
	default:
	}

	select {
	case s.requests <- struct{}{}:
	case <-ctx.Done():
	case <-done:
	default:
	}
}

func (s *serializer) loop(ctx context.Context, done <-chan struct{}, tick func()) {
	ticker := s.ticker.Load()
	if ticker == nil {
		return
	}
	for {
		// Prefer termination when a tick/request and shutdown are both ready.
		// Graceful shutdown performs its own final serialization.
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		default:
		}

		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			if tick != nil {
				tick()
			}
			// Serialize directly. Sending another request from inside the sole
			// request consumer can self-block when one is already pending.
			s.serialize(ctx)
		case <-s.requests:
			s.serialize(ctx)
		}
	}
}

// serialize sends every group aggregated so far to the output and reports
// whether all of them were handed over. It reports false when ctx ended
// before it got the permit or in the middle of the sends, and when no output
// is bound. The groups a periodic serialization could not send are re-merged
// (see mergeRemaining) and go out with the next one.
func (s *serializer) serialize(ctx context.Context) bool {
	remaining, ok := s.send(ctx)
	if len(remaining) > 0 {
		s.mergeRemaining(remaining)
	}
	return ok
}

// serializeFinal is serialize for the final serialization, which has no next
// one: the groups it could not send are dropped instead of re-merged, and
// their number is returned with whether every group was handed over (see
// Aggregate.FinalResultInterrupted). Groups still held because ctx ended
// before the permit was acquired are counted too. The count excludes the
// groups of a periodic serialization still in flight (it holds the permit and
// its own snapshot of them): such a pass sends them, or re-merges the ones it
// could not send, which this count then includes once the merge happened.
func (s *serializer) serializeFinal(ctx context.Context) (unsent int, ok bool) {
	remaining, ok := s.send(ctx)
	if ok {
		return 0, true
	}
	return len(remaining) + s.countGroups(), false
}

// send hands a snapshot of every group aggregated so far to the output. It
// returns the groups it could not send once ctx ended part way through, and
// whether every group was handed over; the caller decides what becomes of
// the unsent groups.
func (s *serializer) send(ctx context.Context) (map[string]*mapr.AggregateSet, bool) {
	if !s.acquire(ctx) {
		return nil, false
	}
	defer s.release()

	output := s.output.Load()
	if output == nil {
		s.logger.Error("Aggregate maprMessages channel is nil")
		return nil, false
	}

	snapshot := s.swapGroupSets()
	if len(snapshot) == 0 {
		return nil, true
	}

	group := mapr.NewGroupSet(s.logger)
	for groupKey, aggregateSet := range snapshot {
		groupSet := group.GetSet(groupKey)
		*groupSet = *aggregateSet
	}

	remaining := group.Serialize(ctx, output.messages)
	return remaining, len(remaining) == 0
}

// aggregateBatch merges the parsed lines of one batch into their groups under
// a single acquisition of the group lock.
func (s *serializer) aggregateBatch(lines []*lineScratch) {
	if len(lines) == 0 {
		return
	}
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	for _, line := range lines {
		s.aggregateLocked(line.fields, line.key)
	}
}

// aggregateLocked merges one parsed line into its group; the caller holds
// groupMu. groupKey is borrowed from the caller's per-line scratch buffer: it
// is only read here, and copied when it has to become a map key.
func (s *serializer) aggregateLocked(fields map[string]string, groupKey []byte) {
	var set *mapr.AggregateSet
	var addedSample, created bool
	for _, sc := range s.query.Select {
		val, ok := fields[sc.Field]
		if !ok {
			continue
		}
		// Allocate the set only after a select field matches. Empty sets would
		// otherwise reach the client as rows of groups no line had a field of.
		if set == nil {
			// Looking up with m[string(b)] takes no allocation; only a group
			// seen for the first time copies the borrowed key, because the
			// caller reuses its buffer for the next line.
			set, ok = s.groupSets[string(groupKey)]
			if !ok {
				set = mapr.NewAggregateSet()
				s.groupSets[string(groupKey)] = set
				created = true
			}
		}
		if err := set.Aggregate(sc.FieldStorage, sc.Operation, val, false); err != nil {
			// err can alias the borrowed line (a *strconv.NumError keeps
			// the value it failed to parse), and the caller recycles that
			// buffer as soon as the line is processed. Format it now
			// rather than handing the alias to the logger. The failed value
			// added nothing, so an avg() of it keeps its value count too.
			s.logger.Error("Aggregate aggregation error", err.Error(),
				"field", sc.Field, "operation", sc.Operation)
			continue
		}
		addedSample = true
	}
	// Samples counts the line once whichever fields it had. avg() does not
	// divide by it but by its own value count (see mapr.AggregateSet); it is
	// still sent for older clients, which divide avg() by it.
	if addedSample {
		set.Samples++
		return
	}
	// Every value of the line failed to aggregate (e.g. sum() of a word): a
	// set created for it holds nothing and would be sent as "key∥0∥", which
	// the client rejects with an error per group and interval.
	if created {
		delete(s.groupSets, string(groupKey))
	}
}

func (s *serializer) acquire(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.permit:
		if ctx.Err() != nil {
			s.release()
			return false
		}
		return true
	}
}

func (s *serializer) release() {
	s.permit <- struct{}{}
}

// mergeRemaining puts the groups an interrupted serialization could not send
// back into the live group sets. A group that was aggregated again meanwhile
// has a newer live set: that one is merged into the older unsent set (and the
// unsent set takes its place), so the client's merge rule applies here too:
// the newer last()/len() value wins, and only fields a set carries are merged.
func (s *serializer) mergeRemaining(remaining map[string]*mapr.AggregateSet) {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	for key, unsent := range remaining {
		live, ok := s.groupSets[key]
		if ok {
			if err := unsent.Merge(s.query, live); err != nil {
				// Only an unknown aggregation fails, which the query parser
				// rejects. Merge then changed neither set: the live set stays
				// and the unsent group is dropped (logged here), as a set
				// that cannot be merged could not be merged by the client
				// either.
				s.logger.Error("Aggregate re-merge failed", err.Error(), "group", key)
				continue
			}
		}
		s.groupSets[key] = unsent
	}
	s.logger.Warn("Aggregate serialize interrupted; re-merged unsent groups",
		"remaining", len(remaining))
}

func (s *serializer) swapGroupSets() map[string]*mapr.AggregateSet {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	if len(s.groupSets) == 0 {
		return nil
	}

	snapshot := s.groupSets
	s.groupSets = make(map[string]*mapr.AggregateSet, len(snapshot))
	return snapshot
}

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

func (s *serializer) serialize(ctx context.Context) {
	if !s.acquire(ctx) {
		return
	}
	defer s.release()

	output := s.output.Load()
	if output == nil {
		s.logger.Error("Aggregate maprMessages channel is nil")
		return
	}

	snapshot := s.swapGroupSets()
	if len(snapshot) == 0 {
		return
	}

	group := mapr.NewGroupSet(s.logger)
	for groupKey, aggregateSet := range snapshot {
		groupSet := group.GetSet(groupKey)
		*groupSet = *aggregateSet
	}

	remaining := group.Serialize(ctx, output.messages)
	if len(remaining) > 0 {
		s.mergeRemaining(remaining)
	}
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
	var addedSample bool
	for _, sc := range s.query.Select {
		val, ok := fields[sc.Field]
		if !ok {
			continue
		}
		// Allocate the set only after a select field matches. Empty sets would
		// otherwise reach the client with Samples==0 and produce NaN for avg().
		if set == nil {
			// Looking up with m[string(b)] takes no allocation; only a group
			// seen for the first time copies the borrowed key, because the
			// caller reuses its buffer for the next line.
			set, ok = s.groupSets[string(groupKey)]
			if !ok {
				set = mapr.NewAggregateSet()
				s.groupSets[string(groupKey)] = set
			}
		}
		if err := set.Aggregate(sc.FieldStorage, sc.Operation, val, false); err != nil {
			// err can alias the borrowed line (a *strconv.NumError keeps
			// the value it failed to parse), and the caller recycles that
			// buffer as soon as the line is processed. Format it now
			// rather than handing the alias to the logger.
			s.logger.Error("Aggregate aggregation error", err.Error(),
				"field", sc.Field, "operation", sc.Operation)
			continue
		}
		addedSample = true
	}
	if addedSample {
		set.Samples++
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

func (s *serializer) mergeRemaining(remaining map[string]*mapr.AggregateSet) {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	for key, set := range remaining {
		existing, ok := s.groupSets[key]
		if !ok {
			s.groupSets[key] = set
			continue
		}
		mergeCancelledSnapshot(s.query, existing, set, s.logger)
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

package mapr

import (
	"context"
	"sync"

	"github.com/mimecast/dtail/internal/logging"
)

// SafeAggregateSet is a thread-safe wrapper around AggregateSet for concurrent aggregation.
// It uses a read-write mutex to allow concurrent reads but exclusive writes.
type SafeAggregateSet struct {
	mu  sync.RWMutex
	set *AggregateSet
}

// NewSafeAggregateSet creates a new thread-safe aggregate set.
func NewSafeAggregateSet() *SafeAggregateSet {
	return &SafeAggregateSet{
		set: NewAggregateSet(),
	}
}

// Aggregate data to the aggregate set with thread safety.
func (s *SafeAggregateSet) Aggregate(key string, agg AggregateOperation, value string, clientAggregation bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.set.Aggregate(key, agg, value, clientAggregation)
}

// IncrementSamples increments the sample count safely.
func (s *SafeAggregateSet) IncrementSamples() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set.Samples++
}

// Clone creates a deep copy of the aggregate set.
// This is useful for serialization to avoid holding locks for too long.
func (s *SafeAggregateSet) Clone() *AggregateSet {
	s.mu.RLock()
	defer s.mu.RUnlock()

	clone := &AggregateSet{
		Samples: s.set.Samples,
		FValues: make(map[string]float64, len(s.set.FValues)),
		SValues: make(map[string]string, len(s.set.SValues)),
	}

	// Deep copy the maps
	for k, v := range s.set.FValues {
		clone.FValues[k] = v
	}
	for k, v := range s.set.SValues {
		clone.SValues[k] = v
	}

	return clone
}

// Serialize the aggregate set safely. Returns whether the message was sent
// before the context was cancelled; mirrors AggregateSet.Serialize.
func (s *SafeAggregateSet) Serialize(ctx context.Context, groupKey string, ch chan<- string,
	logger logging.Logger) bool {
	// Clone the set to avoid holding the lock during serialization
	clone := s.Clone()
	return clone.Serialize(ctx, groupKey, ch, logger)
}

// GetSamples returns the current sample count safely.
func (s *SafeAggregateSet) GetSamples() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.set.Samples
}

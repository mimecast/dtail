package aggregate

import (
	"bytes"
	"errors"
	"sync"
)

type rawLine struct {
	content  *bytes.Buffer
	sourceID string
}

type batcher struct {
	mu      sync.Mutex
	pending []rawLine
	maxSize int
}

func newBatcher(maxSize int) (*batcher, error) {
	if maxSize <= 0 {
		return nil, errors.New("create aggregate batcher: maximum size must be positive")
	}
	return &batcher{
		pending: make([]rawLine, 0, maxSize),
		maxSize: maxSize,
	}, nil
}

// add retains line until the batch reaches its threshold. The caller owns any
// returned batch and may process it without holding the batcher lock.
func (b *batcher) add(line rawLine) []rawLine {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.pending = append(b.pending, line)
	if len(b.pending) < b.maxSize {
		return nil
	}
	return b.takeLocked()
}

// take transfers every pending line to the caller.
func (b *batcher) take() []rawLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.takeLocked()
}

func (b *batcher) takeLocked() []rawLine {
	if len(b.pending) == 0 {
		return nil
	}
	batch := b.pending
	b.pending = make([]rawLine, 0, b.maxSize)
	return batch
}

package handlers

import (
	"context"
	"sync"
	"sync/atomic"

	mapaggregate "github.com/mimecast/dtail/internal/mapr/aggregate"
	"github.com/mimecast/dtail/internal/omode"
)

type commandBatchContextKeyType struct{}

var commandBatchContextKey commandBatchContextKeyType

func withCommandBatch(ctx context.Context, batch *commandBatch) context.Context {
	return context.WithValue(ctx, commandBatchContextKey, batch)
}

func commandBatchFromContext(ctx context.Context) *commandBatch {
	batch, _ := ctx.Value(commandBatchContextKey).(*commandBatch)
	return batch
}

// commandBatch coordinates completion of a bounded command stream. Read
// handlers are launched asynchronously, so the FIFO completion
// marker can arrive after every read was admitted but before those reads have
// registered their files in pendingFiles. Count one-shot read commands at
// admission time to close that gap.
type commandBatch struct {
	mu      sync.Mutex
	enabled bool
	open    atomic.Bool
	reads   map[*mapaggregate.Aggregate]int
}

type commandBatchRead struct {
	aggregate *mapaggregate.Aggregate
	tracked   bool
}

func (b *commandBatch) begin() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.enabled {
		return
	}
	b.enabled = true
	b.reads = make(map[*mapaggregate.Aggregate]int)
	b.open.Store(true)
}

func (b *commandBatch) beginRead(mode omode.Mode, aggregate *mapaggregate.Aggregate) commandBatchRead {
	if mode != omode.CatClient && mode != omode.GrepClient {
		return commandBatchRead{}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	// The FIFO marker or SESSION dispatch scope bounds ownership to commands
	// admitted while this batch is open.
	if !b.enabled || !b.open.Load() {
		return commandBatchRead{}
	}
	b.reads[aggregate]++
	return commandBatchRead{aggregate: aggregate, tracked: true}
}

// completeRead returns the exact aggregate whose admitted reads have drained.
// Capturing aggregate identity at admission prevents an old interactive
// generation from finishing a replacement aggregate after SESSION UPDATE.
func (b *commandBatch) completeRead(read commandBatchRead) *mapaggregate.Aggregate {
	if !read.tracked {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	remaining := b.reads[read.aggregate] - 1
	if remaining > 0 {
		b.reads[read.aggregate] = remaining
		return nil
	}
	b.reads[read.aggregate] = 0
	if b.open.Load() {
		return nil
	}

	delete(b.reads, read.aggregate)
	b.retireIfDrained()
	return read.aggregate
}

// complete closes admission and returns aggregates whose reads had already
// drained. Aggregates with active reads remain owned until completeRead.
func (b *commandBatch) complete() []*mapaggregate.Aggregate {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.open.Store(false)
	ready := make([]*mapaggregate.Aggregate, 0, len(b.reads))
	for aggregate, remaining := range b.reads {
		if remaining != 0 {
			continue
		}
		if aggregate != nil {
			ready = append(ready, aggregate)
		}
		delete(b.reads, aggregate)
	}
	b.retireIfDrained()
	return ready
}

// ownsAggregate reports whether this initial batch owns completion for the
// given aggregate. Later interactive generations are deliberately absent.
func (b *commandBatch) ownsAggregate(aggregate *mapaggregate.Aggregate) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.enabled {
		return false
	}
	_, owned := b.reads[aggregate]
	return owned
}

func (b *commandBatch) retireIfDrained() {
	if !b.open.Load() && len(b.reads) == 0 {
		b.enabled = false
	}
}

func (b *commandBatch) isOpen() bool {
	return b.open.Load()
}

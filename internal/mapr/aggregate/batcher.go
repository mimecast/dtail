package aggregate

import (
	"bytes"

	"github.com/mimecast/dtail/internal/io/pool"
)

// processorBatchSize is the number of lines a Processor collects before it
// aggregates them in one go.
const processorBatchSize = 100

type rawLine struct {
	content  *bytes.Buffer
	sourceID string
}

// lineBatch collects the lines of a single Processor. A processor is fed by
// exactly one reader goroutine, which also flushes and closes it, so the batch
// needs no lock; the backing array is reused for every batch.
type lineBatch struct {
	lines []rawLine
}

// add appends line and reports whether the batch has reached its threshold.
func (b *lineBatch) add(line rawLine) bool {
	if b.lines == nil {
		// Allocated on first use: processors which never see a line, such
		// as inert ones created after cancellation, cost no batch storage.
		b.lines = make([]rawLine, 0, processorBatchSize)
	}
	b.lines = append(b.lines, line)
	return len(b.lines) >= processorBatchSize
}

// pending returns the batched lines. They stay owned by the batch until reset.
func (b *lineBatch) pending() []rawLine {
	return b.lines
}

// reset empties the batch for reuse and drops its references to line buffers
// the caller has recycled meanwhile.
func (b *lineBatch) reset() {
	clear(b.lines)
	b.lines = b.lines[:0]
}

// recycleRawLines returns the line buffers of batch to the buffer pool.
func recycleRawLines(batch []rawLine) {
	for i := range batch {
		if batch[i].content != nil {
			pool.RecycleBytesBuffer(batch[i].content)
		}
	}
}

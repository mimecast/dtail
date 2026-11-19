package aggregate

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/logging"
)

// The colors are all six bytes long so that overwriting a recycled buffer
// with a same-length line corrupts a retained view without changing any
// length, which is the subtlest form the bug could take.
var retentionLines = []string{
	"INFO|1002-071143|1|stats.go:56|8|15|7|0.21|471h0m21s|MAPREDUCE:STATS|host=alpha|color=orange",
	"INFO|1002-071144|1|stats.go:56|8|16|7|0.21|471h0m21s|MAPREDUCE:STATS|host=beta|color=purple",
	"INFO|1002-071145|1|stats.go:56|8|17|7|0.21|471h0m21s|MAPREDUCE:STATS|host=alpha|color=violet",
}

// TestAggregateDoesNotRetainRecycledLineBuffers is the guard for the central
// hazard of the allocation-free line path: the parsed fields, the group key
// and the line itself all borrow a pooled bytes.Buffer that the aggregator
// recycles as soon as the line has been processed. Everything the aggregation
// keeps — group keys, last() and len() strings — must therefore be a copy.
//
// The test feeds lines from pooled buffers, keeps hold of those buffers and
// overwrites them after the batch has been processed, which is exactly what
// the next reader taking them out of the pool does. Any retained view shows
// up as an 'X' in the aggregated state.
func TestAggregateDoesNotRetainRecycledLineBuffers(t *testing.T) {
	const query = `from STATS select count($line),last(color),len(color) group by host`

	aggregate, err := newAggregateFromTextForTest(query, logging.NopLogger{})
	if err != nil {
		t.Fatalf("Failed to create aggregate: %v", err)
	}

	processor := NewProcessor(aggregate, "retention")
	handed := make([]*bytes.Buffer, 0, len(retentionLines))
	for i, line := range retentionLines {
		buffer := pool.BytesBuffer.Get().(*bytes.Buffer)
		buffer.Reset()
		buffer.WriteString(line)
		handed = append(handed, buffer)
		if err := processor.ProcessLine(buffer, uint64(i+1), "retention"); err != nil {
			t.Fatalf("ProcessLine() error = %v", err)
		}
	}
	if err := processor.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if err := processor.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	for i, buffer := range handed {
		buffer.Reset()
		buffer.WriteString(strings.Repeat("X", len(retentionLines[i])))
	}
	// The poisoned buffers are back in the pool, whose users expect empty
	// buffers (RecycleBytesBuffer resets before Put). Empty them again once
	// the checks are done, or a later test's pool.BytesBuffer.Get, such as a
	// follow reader's partial-line buffer, starts with the poison.
	defer func() {
		for _, buffer := range handed {
			buffer.Reset()
		}
	}()

	wantLast := map[string]string{"alpha": "violet", "beta": "purple"}
	wantSamples := map[string]int{"alpha": 2, "beta": 1}

	aggregate.serializer.groupMu.Lock()
	defer aggregate.serializer.groupMu.Unlock()

	if len(aggregate.serializer.groupSets) != len(wantLast) {
		t.Fatalf("aggregated %d groups, want %d: %v", len(aggregate.serializer.groupSets),
			len(wantLast), aggregate.serializer.groupSets)
	}
	for groupKey, set := range aggregate.serializer.groupSets {
		if strings.ContainsRune(groupKey, 'X') {
			t.Errorf("group key %q is a view into a recycled line buffer", groupKey)
			continue
		}
		last, known := wantLast[groupKey]
		if !known {
			t.Errorf("unexpected group %q", groupKey)
			continue
		}
		if set.Samples != wantSamples[groupKey] {
			t.Errorf("group %q has %d samples, want %d", groupKey, set.Samples,
				wantSamples[groupKey])
		}
		lastFound := false
		for storage, value := range set.SValues {
			if strings.ContainsRune(value, 'X') {
				t.Errorf("group %q retained %s=%q, a view into a recycled line buffer",
					groupKey, storage, value)
			}
			if value == last {
				lastFound = true
			}
		}
		if !lastFound {
			t.Errorf("group %q SValues = %v, want %q among them", groupKey, set.SValues, last)
		}
		for storage, value := range set.FValues {
			if strings.HasPrefix(storage, "len(") && value != float64(len(last)) {
				t.Errorf("group %q has %s=%v, want %v", groupKey, storage, value,
					float64(len(last)))
			}
		}
	}
}

// TestProcessorBatchIsAllocationFree pins the point of the allocation-free
// line path: a batch of lines joining groups that already exist must not
// allocate at all, from ProcessLine through the batch merge. A regression (a
// per-line fields map, a copied line, a copied group key, a new batch slice)
// shows up here as a nonzero allocation count.
func TestProcessorBatchIsAllocationFree(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector drops sync.Pool items at random, so pooled " +
			"line buffers and batch scratches are reallocated")
	}
	// One P keeps every batch on the same pooled batch scratch and line
	// buffers. testing.AllocsPerRun switches to one P only for the measured
	// runs, and resizing the Ps drops the per-P pool contents the warm-up
	// left behind, so without this the measured batches reallocate them.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	const query = `from STATS select count($line),sum($goroutines) group by host`

	aggregate, err := newAggregateFromTextForTest(query, logging.NopLogger{})
	if err != nil {
		t.Fatalf("Failed to create aggregate: %v", err)
	}
	processor := NewProcessor(aggregate, "alloc")
	defer func() {
		if err := processor.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()

	// One full batch per run. The line buffers come from the pool the
	// processor recycles them into, as they do for a file reader.
	feedBatch := func() {
		for i := 0; i < processorBatchSize; i++ {
			buffer := pool.BytesBuffer.Get().(*bytes.Buffer)
			buffer.Reset()
			buffer.WriteString(retentionLines[i%len(retentionLines)])
			if err := processor.ProcessLine(buffer, uint64(i+1), "alloc"); err != nil {
				t.Fatalf("ProcessLine() error = %v", err)
			}
		}
	}

	// Warm up: the first batch allocates the groups, the batch storage, the
	// batch scratch and the pooled line buffers. The pooled batch scratch may
	// come from an earlier test whose larger lines stay in its retention
	// history for retentionHistory batches, after which their storage is
	// trimmed, so the warm-up covers that many batches.
	for i := 0; i <= retentionHistory; i++ {
		feedBatch()
	}
	if got := aggregate.countGroups(); got != 2 {
		t.Fatalf("a full batch produced %d groups, want 2 without a Flush", got)
	}

	allocs := testing.AllocsPerRun(20, feedBatch)
	if allocs != 0 {
		t.Errorf("a batch of %d lines made %.1f allocations, want 0",
			processorBatchSize, allocs)
	}
}

// TestProcessorKeepsBufferContract pins that the aggregate Processor does not
// implement line.RawProcessor. It keeps each line's pooled buffer until its
// batch is aggregated, so it cannot borrow the reader's transient slice; the
// file reader must keep handing it owned buffers through ProcessLine.
func TestProcessorKeepsBufferContract(t *testing.T) {
	var processor any = &Processor{}
	if _, ok := processor.(line.RawProcessor); ok {
		t.Fatal("aggregate Processor implements line.RawProcessor, but it retains lines " +
			"beyond the call and must stay on the owned-buffer ProcessLine path")
	}
}

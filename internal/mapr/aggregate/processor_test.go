package aggregate

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/logging"
)

const processorTestQuery = `from STATS select count($line) group by host`

// samplesOf returns the sample count aggregated for group so far.
func samplesOf(aggregate *Aggregate, group string) int {
	aggregate.serializer.groupMu.Lock()
	defer aggregate.serializer.groupMu.Unlock()
	if set, ok := aggregate.serializer.groupSets[group]; ok {
		return set.Samples
	}
	return 0
}

func feedLines(t *testing.T, processor *Processor, lines ...string) {
	t.Helper()
	for i, line := range lines {
		if err := processor.ProcessLine(bytes.NewBufferString(line), uint64(i+1), "test"); err != nil {
			t.Fatalf("ProcessLine() error = %v", err)
		}
	}
}

// TestProcessorFlushDrainsEveryPartialBatch pins the follow-mode contract: a
// follow reader flushes after every read, and every one of those flushes, not
// only the first, must hand the partial batch to the aggregate.
func TestProcessorFlushDrainsEveryPartialBatch(t *testing.T) {
	aggregate, err := newAggregateFromTextForTest(processorTestQuery, logging.NopLogger{})
	if err != nil {
		t.Fatalf("Failed to create aggregate: %v", err)
	}
	processor := NewProcessor(aggregate, "follow")

	feedLines(t, processor, retentionLines[0], retentionLines[2])
	if got := samplesOf(aggregate, "alpha"); got != 0 {
		t.Fatalf("a partial batch reached the aggregate before Flush: %d samples", got)
	}
	for round, want := range []int{2, 3, 4} {
		if round > 0 {
			feedLines(t, processor, retentionLines[0])
		}
		if err := processor.Flush(); err != nil {
			t.Fatalf("Flush() error = %v", err)
		}
		if got := samplesOf(aggregate, "alpha"); got != want {
			t.Fatalf("flush %d: alpha has %d samples, want %d", round+1, got, want)
		}
	}

	if err := processor.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := aggregate.filesProcessed.Load(); got != 1 {
		t.Errorf("filesProcessed = %d after several flushes, want 1", got)
	}
	if got := aggregate.linesProcessed.Load(); got != 4 {
		t.Errorf("linesProcessed = %d, want 4", got)
	}
}

// TestProcessorPartialBatchReachesFinalSerialization covers graceful shutdown
// racing a reader: lines a processor accepted before shutdown began are still
// batched when the aggregate stops, and Close must aggregate them before the
// final serialization, as the former shared batch drain did.
func TestProcessorPartialBatchReachesFinalSerialization(t *testing.T) {
	aggregate, err := newAggregateFromTextForTest(processorTestQuery, logging.NopLogger{})
	if err != nil {
		t.Fatalf("Failed to create aggregate: %v", err)
	}
	messages := make(chan string, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		aggregate.Start(ctx, messages)
	}()
	waitForAggregateStart(t, aggregate)

	processor := NewProcessor(aggregate, "partial")
	feedLines(t, processor, retentionLines...)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		aggregate.Shutdown(context.Background())
	}()
	waitForAggregateCondition(t, 2*time.Second, "shutdown did not stop the aggregate",
		aggregate.stopping)

	// The reader notices the cancellation and closes its processor.
	if err := processor.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not complete after the processor closed")
	}
	cancel()
	<-startDone
	close(messages)

	var results []string
	for message := range messages {
		results = append(results, message)
	}
	joined := strings.Join(results, "\n")
	if !strings.Contains(joined, "alpha") || !strings.Contains(joined, "beta") {
		t.Fatalf("final serialization %q lacks the batched lines", joined)
	}
}

// TestProcessorAbortDiscardsPartialBatch checks that output-free termination
// does not aggregate lines still batched in a processor.
func TestProcessorAbortDiscardsPartialBatch(t *testing.T) {
	aggregate, err := newAggregateFromTextForTest(processorTestQuery, logging.NopLogger{})
	if err != nil {
		t.Fatalf("Failed to create aggregate: %v", err)
	}
	processor := NewProcessor(aggregate, "abort")
	feedLines(t, processor, retentionLines...)

	aggregate.Abort()
	if err := processor.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := aggregate.countGroups(); got != 0 {
		t.Errorf("aborted aggregate holds %d groups, want 0", got)
	}
	if got := len(processor.batch.pending()); got != 0 {
		t.Errorf("Close left %d lines batched", got)
	}
	if got := aggregate.linesProcessed.Load(); got != 0 {
		t.Errorf("linesProcessed = %d after the batch was discarded, want 0", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	aggregate.AbortAndWait(ctx)
	if ctx.Err() != nil {
		t.Fatal("AbortAndWait did not join the closed processor")
	}
}

// TestProcessorsMergePartialBatchesConcurrently runs many processors at once
// whose line counts are not multiples of the batch size, so every one of them
// ends with a partial batch that only Flush or Close hands to the aggregate
// while other processors are still merging full batches. Half of them also
// flush part way through, as a follow reader does after each read. The final
// per-group counts must be exact.
func TestProcessorsMergePartialBatchesConcurrently(t *testing.T) {
	aggregate, err := newAggregateFromTextForTest(processorTestQuery, logging.NopLogger{})
	if err != nil {
		t.Fatalf("Failed to create aggregate: %v", err)
	}

	const processors = 16
	want := map[string]int{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for p := 0; p < processors; p++ {
		lines := 37 + 61*p // 37, 98, 159, ... never a multiple of 100
		if lines%processorBatchSize == 0 {
			t.Fatalf("test setup: %d lines is a multiple of the batch size", lines)
		}
		for i := 0; i < lines; i++ {
			want[retentionHost(i)]++
		}
		wg.Add(1)
		go func(p, lines int) {
			defer wg.Done()
			processor := NewProcessor(aggregate, "concurrent")
			<-start
			for i := 0; i < lines; i++ {
				line := retentionLines[i%len(retentionLines)]
				if err := processor.ProcessLine(bytes.NewBufferString(line),
					uint64(i+1), "concurrent"); err != nil {
					t.Errorf("ProcessLine() error = %v", err)
					return
				}
				if p%2 == 1 && i%45 == 44 {
					if err := processor.Flush(); err != nil {
						t.Errorf("Flush() error = %v", err)
						return
					}
				}
			}
			if p%3 == 0 {
				// Close alone must drain the partial batch too.
				if err := processor.Close(); err != nil {
					t.Errorf("Close() error = %v", err)
				}
				return
			}
			if err := processor.Flush(); err != nil {
				t.Errorf("Flush() error = %v", err)
			}
			if err := processor.Close(); err != nil {
				t.Errorf("Close() error = %v", err)
			}
		}(p, lines)
	}
	close(start)
	wg.Wait()

	total := 0
	for group, count := range want {
		total += count
		if got := samplesOf(aggregate, group); got != count {
			t.Errorf("group %q has %d samples, want %d", group, got, count)
		}
	}
	if got := aggregate.countGroups(); got != len(want) {
		t.Errorf("aggregate holds %d groups, want %d", got, len(want))
	}
	if got := aggregate.linesProcessed.Load(); got != uint64(total) {
		t.Errorf("linesProcessed = %d, want %d", got, total)
	}
	if got := aggregate.filesProcessed.Load(); got != processors {
		t.Errorf("filesProcessed = %d, want %d", got, processors)
	}
}

// retentionHost is the host of retentionLines[i % len(retentionLines)].
func retentionHost(i int) string {
	if i%len(retentionLines) == 1 {
		return "beta"
	}
	return "alpha"
}

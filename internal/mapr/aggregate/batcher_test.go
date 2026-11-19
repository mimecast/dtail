package aggregate

import (
	"bytes"
	"strconv"
	"sync"
	"testing"
)

func TestNewBatcherRejectsInvalidSize(t *testing.T) {
	for _, size := range []int{-1, 0} {
		if _, err := newBatcher(size); err == nil {
			t.Fatalf("newBatcher(%d) succeeded, want error", size)
		}
	}
}

func TestBatcherTransfersFullAndPartialBatches(t *testing.T) {
	b, err := newBatcher(2)
	if err != nil {
		t.Fatalf("newBatcher: %v", err)
	}
	first := rawLine{content: bytes.NewBufferString("first"), sourceID: "one"}
	second := rawLine{content: bytes.NewBufferString("second"), sourceID: "two"}
	third := rawLine{content: bytes.NewBufferString("third"), sourceID: "three"}

	if batch := b.add(first); batch != nil {
		t.Fatalf("first add returned batch of length %d", len(batch))
	}
	batch := b.add(second)
	if len(batch) != 2 || batch[0].sourceID != "one" || batch[1].sourceID != "two" {
		t.Fatalf("full batch = %#v, want first two lines in order", batch)
	}
	if pending := b.add(third); pending != nil {
		t.Fatalf("third add returned batch of length %d", len(pending))
	}
	batch = b.take()
	if len(batch) != 1 || batch[0].sourceID != "three" {
		t.Fatalf("partial batch = %#v, want third line", batch)
	}
	if batch := b.take(); batch != nil {
		t.Fatalf("empty take returned batch of length %d", len(batch))
	}
}

func TestBatcherConcurrentAddsDoNotLoseLines(t *testing.T) {
	const lineCount = 1000
	b, err := newBatcher(17)
	if err != nil {
		t.Fatalf("newBatcher: %v", err)
	}

	var wg sync.WaitGroup
	var batchesMu sync.Mutex
	var batches [][]rawLine
	for i := 0; i < lineCount; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			batch := b.add(rawLine{sourceID: strconv.Itoa(id)})
			if len(batch) == 0 {
				return
			}
			batchesMu.Lock()
			batches = append(batches, batch)
			batchesMu.Unlock()
		}(i)
	}
	wg.Wait()
	batches = append(batches, b.take())

	seen := make(map[string]struct{}, lineCount)
	for _, batch := range batches {
		for _, line := range batch {
			if _, duplicate := seen[line.sourceID]; duplicate {
				t.Fatalf("line %s appeared more than once", line.sourceID)
			}
			seen[line.sourceID] = struct{}{}
		}
	}
	if len(seen) != lineCount {
		t.Fatalf("collected %d lines, want %d", len(seen), lineCount)
	}
}

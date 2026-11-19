package aggregate

import (
	"bytes"
	"reflect"
	"strconv"
	"testing"
)

func TestLineBatchSignalsThresholdAndReusesStorage(t *testing.T) {
	var batch lineBatch
	if batch.pending() != nil {
		t.Fatal("a fresh batch allocated storage before its first line")
	}

	for i := 0; i < processorBatchSize-1; i++ {
		if batch.add(rawLine{sourceID: strconv.Itoa(i)}) {
			t.Fatalf("add reported a full batch after %d lines, want %d", i+1, processorBatchSize)
		}
	}
	if !batch.add(rawLine{sourceID: "last"}) {
		t.Fatalf("add did not report a full batch at %d lines", processorBatchSize)
	}
	lines := batch.pending()
	if len(lines) != processorBatchSize || lines[0].sourceID != "0" ||
		lines[processorBatchSize-1].sourceID != "last" {
		t.Fatalf("pending() returned %d lines out of order", len(lines))
	}
	backing := reflect.ValueOf(lines).Pointer()

	lines[0].content = bytes.NewBufferString("recycled")
	batch.reset()
	if len(batch.pending()) != 0 {
		t.Fatalf("reset left %d lines pending", len(batch.pending()))
	}
	if lines[0].content != nil {
		t.Error("reset kept a reference to a recycled line buffer")
	}

	batch.add(rawLine{sourceID: "next"})
	if reflect.ValueOf(batch.pending()).Pointer() != backing {
		t.Error("the batch did not reuse its backing array after reset")
	}
}

func TestRecycleRawLinesSkipsNilContent(t *testing.T) {
	recycleRawLines([]rawLine{{content: nil}, {content: bytes.NewBufferString("line")}})
}

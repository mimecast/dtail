package handlers

import (
	"bytes"
	"testing"

	"github.com/mimecast/dtail/internal/io/pool"
)

// nopLineWriter is a zero-allocation LineWriter used to isolate the
// per-line trace-guard overhead in the alloc test below.
type nopLineWriter struct{}

func (nopLineWriter) WriteLineData(lineContent []byte, lineNum uint64, sourceID string) error {
	return nil
}
func (nopLineWriter) WriteServerMessage(message string) error { return nil }
func (nopLineWriter) Flush() error                            { return nil }

// TestDirectLineProcessorProcessLineNoAllocWhenTraceOff locks in task 1t0: the
// per-line hot path must not allocate when trace logging is off (the default
// production/output level). Before the fix, ProcessLine unconditionally built a
// []any and boxed lineCount/lineNum (runtime.convT64) and sourceID
// (convTstring) on every line even though Trace early-returns — that call
// site was ~98% of all allocated objects in the output serverless dcat profile.
// The injected logger's TraceEnabled guard elides all of it.
func TestDirectLineProcessorProcessLineNoAllocWhenTraceOff(t *testing.T) {
	if handlerTestLogger.TraceEnabled() {
		t.Fatal("precondition failed: trace must be disabled for this test")
	}

	p := NewDirectLineProcessor(nopLineWriter{}, "globID", handlerTestLogger)

	allocs := testing.AllocsPerRun(1000, func() {
		// Mirror the real hot path: a pooled buffer that ProcessLine recycles.
		buf := pool.BytesBuffer.Get().(*bytes.Buffer)
		buf.Reset()
		buf.WriteString("some representative log line content")
		if err := p.ProcessLine(buf, 42, "sourceID"); err != nil {
			t.Fatalf("ProcessLine: %v", err)
		}
	})

	if allocs != 0 {
		t.Fatalf("ProcessLine allocated %v objects/run with trace off; want 0 "+
			"(the per-line trace guard should elide all interface boxing)", allocs)
	}
}

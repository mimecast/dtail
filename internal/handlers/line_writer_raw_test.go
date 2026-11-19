package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/pool"
)

// rawPathTestLines covers the shapes the readers hand over: scanner tokens with
// their newline, follow-mode lines without it, an empty line and a line longer
// than the pooled buffers' usual size.
var rawPathTestLines = []string{
	"first line\n",
	"follow line without newline",
	"",
	"\n",
	strings.Repeat("long ", 4000) + "\n",
	"last ERROR line\n",
}

type rawPathWriterCase struct {
	name string
	// newWriter returns a LineWriter plus a function that flushes it and returns
	// everything it emitted.
	newWriter func(t *testing.T) (LineWriter, func() []byte)
}

func rawPathWriterCases() []rawPathWriterCase {
	direct := func(plain, serverless bool) func(t *testing.T) (LineWriter, func() []byte) {
		return func(t *testing.T) (LineWriter, func() []byte) {
			var out bytes.Buffer
			w := NewDirectWriter(&out, "testhost", plain, serverless)
			return w, func() []byte {
				if err := w.Flush(); err != nil {
					t.Fatalf("Flush: %v", err)
				}
				return out.Bytes()
			}
		}
	}
	network := func(plain bool) func(t *testing.T) (LineWriter, func() []byte) {
		return func(t *testing.T) (LineWriter, func() []byte) {
			outputLines := make(chan []byte, 64)
			w := NewNetworkWriter(context.Background(), outputLines, nil, "testhost", plain, false,
				0, nil, handlerTestLogger)
			return w, func() []byte {
				if err := w.Flush(); err != nil {
					t.Fatalf("Flush: %v", err)
				}
				close(outputLines)
				var out []byte
				for payload := range outputLines {
					out = append(out, payload...)
				}
				return out
			}
		}
	}
	return []rawPathWriterCase{
		{name: "direct serverless plain (dcat --plain)", newWriter: direct(true, true)},
		{name: "direct serverless colored", newWriter: direct(false, true)},
		{name: "direct server plain", newWriter: direct(true, false)},
		{name: "direct server protocol", newWriter: direct(false, false)},
		{name: "network plain", newWriter: network(true)},
		{name: "network protocol", newWriter: network(false)},
	}
}

// TestDirectLineProcessorProcessRawLineMatchesProcessLine checks that the raw
// fast path emits exactly the bytes of the buffer path for every writer format,
// and that it does not retain the borrowed slice: the caller overwrites the
// slice right after each call (as the next Scan would), before the writer has
// flushed anything.
func TestDirectLineProcessorProcessRawLineMatchesProcessLine(t *testing.T) {
	previousClient := config.Client
	config.Client = nil
	t.Cleanup(func() { config.Client = previousClient })

	for _, tc := range rawPathWriterCases() {
		t.Run(tc.name, func(t *testing.T) {
			bufferWriter, bufferOutput := tc.newWriter(t)
			bufferProcessor := NewDirectLineProcessor(bufferWriter, "glob", handlerTestLogger)
			rawWriter, rawOutput := tc.newWriter(t)
			rawProcessor := NewDirectLineProcessor(rawWriter, "glob", handlerTestLogger)

			scratch := make([]byte, 0, 64)
			for i, content := range rawPathTestLines {
				lineNum := uint64(i + 1)

				buf := pool.BytesBuffer.Get().(*bytes.Buffer)
				buf.Reset()
				buf.WriteString(content)
				if err := bufferProcessor.ProcessLine(buf, lineNum, "source.log"); err != nil {
					t.Fatalf("ProcessLine(%d): %v", lineNum, err)
				}

				scratch = append(scratch[:0], content...)
				if err := rawProcessor.ProcessRawLine(scratch, lineNum, "source.log"); err != nil {
					t.Fatalf("ProcessRawLine(%d): %v", lineNum, err)
				}
				for j := range scratch {
					scratch[j] = 'X'
				}
			}

			want := bufferOutput()
			got := rawOutput()
			if len(want) == 0 {
				t.Fatal("buffer path emitted nothing; the comparison would be vacuous")
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("raw path output differs from buffer path output\n got: %q\nwant: %q",
					truncateForLog(got), truncateForLog(want))
			}
			if rawProcessor.lineCount != uint64(len(rawPathTestLines)) {
				t.Fatalf("lineCount = %d, want %d", rawProcessor.lineCount, len(rawPathTestLines))
			}
		})
	}
}

// TestDirectLineProcessorProcessRawLineNoAllocWhenTraceOff is the raw-path
// counterpart of the ProcessLine alloc test: no pooled buffer and no boxing.
func TestDirectLineProcessorProcessRawLineNoAllocWhenTraceOff(t *testing.T) {
	if handlerTestLogger.TraceEnabled() {
		t.Fatal("precondition failed: trace must be disabled for this test")
	}

	p := NewDirectLineProcessor(nopLineWriter{}, "globID", handlerTestLogger)
	raw := []byte("some representative log line content\n")
	allocs := testing.AllocsPerRun(1000, func() {
		if err := p.ProcessRawLine(raw, 42, "sourceID"); err != nil {
			t.Fatalf("ProcessRawLine: %v", err)
		}
	})
	if allocs != 0 {
		t.Fatalf("ProcessRawLine allocated %v objects/run with trace off; want 0", allocs)
	}
}

type failingLineWriter struct {
	nopLineWriter
	err error
}

func (w failingLineWriter) WriteLineData([]byte, uint64, string) error { return w.err }

// TestDirectLineProcessorProcessRawLineReturnsWriterError checks that a write
// error surfaces unchanged and that the borrowed slice is left alone.
func TestDirectLineProcessorProcessRawLineReturnsWriterError(t *testing.T) {
	sinkErr := errors.New("broken pipe")
	p := NewDirectLineProcessor(failingLineWriter{err: sinkErr}, "glob", handlerTestLogger)

	raw := []byte("payload\n")
	if err := p.ProcessRawLine(raw, 1, "source"); !errors.Is(err, sinkErr) {
		t.Fatalf("ProcessRawLine error = %v, want %v", err, sinkErr)
	}
	if string(raw) != "payload\n" {
		t.Fatalf("borrowed slice modified to %q", raw)
	}
}

func truncateForLog(b []byte) []byte {
	const limit = 200
	if len(b) > limit {
		return b[:limit]
	}
	return b
}

// BenchmarkDirectLineProcessorLinePath compares the per-line cost of the two
// entry points into a plain serverless DirectWriter: "buffer" is what the file
// reader did before line.RawProcessor (pooled buffer, copy, ProcessLine,
// recycle), "raw" is the borrowed-slice fast path.
func BenchmarkDirectLineProcessorLinePath(b *testing.B) {
	line := []byte("2026-01-01T00:00:07Z INFO request=000000007 user007 path=/api/item/0007 " +
		"status=200 payload=abcdefghijklmnopqrstuvwxyz0123456789\n")

	b.Run("buffer", func(b *testing.B) {
		p := NewDirectLineProcessor(NewDirectWriter(io.Discard, "host", true, true), "glob", handlerTestLogger)
		b.SetBytes(int64(len(line)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf := pool.BytesBuffer.Get().(*bytes.Buffer)
			buf.Write(line)
			if err := p.ProcessLine(buf, uint64(i), "source.log"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("raw", func(b *testing.B) {
		p := NewDirectLineProcessor(NewDirectWriter(io.Discard, "host", true, true), "glob", handlerTestLogger)
		b.SetBytes(int64(len(line)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := p.ProcessRawLine(line, uint64(i), "source.log"); err != nil {
				b.Fatal(err)
			}
		}
	})
}

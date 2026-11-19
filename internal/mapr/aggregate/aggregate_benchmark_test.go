package aggregate

import (
	"bytes"
	"fmt"
	"sync"
	"testing"

	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/logging"
)

const benchmarkAggregateQuery = `from STATS select count($line),avg($goroutines),` +
	`max($goroutines),sum($goroutines) group by $goroutines`

// benchmarkStatsLines returns lines shaped like the benchmark script's stats
// log, spread over a few dozen groups.
func benchmarkStatsLines(count int) []string {
	lines := make([]string, count)
	for i := range lines {
		lines[i] = fmt.Sprintf("INFO|0626-140021|1|stats.go:56|%d|%d|%d|0.01|1h0m0s|"+
			"MAPREDUCE:STATS|hostname=host%d|currentConnections=%d|lifetimeConnections=%d",
			i%7, i%40, i%13, i%10, i%5, 1000+i)
	}
	return lines
}

// feedProcessor hands every line to processor the way a file reader does: a
// pooled buffer per line, which the processor owns and recycles. It runs on
// its own goroutine, so it reports failures with Errorf rather than Fatalf.
func feedProcessor(b *testing.B, processor *Processor, lines []string, n int) {
	for i := 0; i < n; i++ {
		buf := pool.BytesBuffer.Get().(*bytes.Buffer)
		buf.Reset()
		buf.WriteString(lines[i%len(lines)])
		if err := processor.ProcessLine(buf, uint64(i), "bench"); err != nil {
			b.Errorf("ProcessLine: %v", err)
			return
		}
	}
	if err := processor.Close(); err != nil {
		b.Errorf("Close: %v", err)
	}
}

// BenchmarkProcessorProcessLine measures the per-line aggregation path, with
// one processor (one input file) and with several processors feeding the same
// aggregate concurrently (several input files).
func BenchmarkProcessorProcessLine(b *testing.B) {
	lines := benchmarkStatsLines(1000)
	for _, processors := range []int{1, 4} {
		b.Run(fmt.Sprintf("processors_%d", processors), func(b *testing.B) {
			agg, err := newAggregateFromTextForTest(benchmarkAggregateQuery, logging.NopLogger{})
			if err != nil {
				b.Fatalf("create aggregate: %v", err)
			}
			perProcessor := b.N / processors
			b.ReportAllocs()
			b.ResetTimer()

			var wg sync.WaitGroup
			for p := 0; p < processors; p++ {
				processor := NewProcessor(agg, fmt.Sprintf("file%d", p))
				wg.Add(1)
				go func() {
					defer wg.Done()
					feedProcessor(b, processor, lines, perProcessor)
				}()
			}
			wg.Wait()
			b.StopTimer()
			if got, want := agg.linesProcessed.Load(), uint64(perProcessor*processors); got != want {
				b.Fatalf("processed %d lines, want %d", got, want)
			}
		})
	}
}

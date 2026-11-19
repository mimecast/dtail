package loggers

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestIdleLoggerCPU is an opt-in process-CPU probe, not a timing assertion in
// the normal suite. Run it alone, without -race, with DTAIL_PERF_IDLE_PROBE=yes.
func TestIdleLoggerCPU(t *testing.T) {
	if os.Getenv("DTAIL_PERF_IDLE_PROBE") != "yes" {
		t.Skip("opt-in isolated idle CPU measurement")
	}
	for _, count := range []int{0, 1, 16} {
		t.Run(fmt.Sprintf("pairs%d", count), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			defer func() { cancel(); wg.Wait() }()
			clocks := make([]int, count)
			for i := range count {
				f := newFile(Strategy{Rotation: DailyRotation}, "")
				f.clock = func() time.Time { clocks[i]++; return time.Now() }
				wg.Add(2)
				f.Start(ctx, &wg)
				newStdoutWriter(io.Discard).Start(ctx, &wg)
			}
			time.Sleep(100 * time.Millisecond)
			start, before := time.Now(), idleLoggerCPU(t)
			time.Sleep(3 * time.Second)
			cpu, elapsed := idleLoggerCPU(t)-before, time.Since(start)
			cancel()
			wg.Wait()
			reads := 0
			for _, n := range clocks {
				reads += n
			}
			t.Logf("pairs=%d cpu_ms=%.3f elapsed_ms=%.3f core_percent=%.3f file_clock_calls=%d",
				count, float64(cpu)/1e6, float64(elapsed)/1e6, 100*float64(cpu)/float64(elapsed), reads)
		})
	}
}

func idleLoggerCPU(t *testing.T) int64 {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	return (usage.Utime.Sec+usage.Stime.Sec)*1e9 + (usage.Utime.Usec+usage.Stime.Usec)*1000
}

//go:build linux

package journal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	journaltest "github.com/mimecast/dtail/internal/io/journal/testhelper"
)

// childProcesses returns the PIDs of this process' child processes, from
// /proc/<pid>/task/<tid>/children of every thread.
func childProcesses(t *testing.T) string {
	t.Helper()
	pid := os.Getpid()
	tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		t.Fatalf("list threads: %v", err)
	}
	var children []string
	for _, task := range tasks {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/children", pid, task.Name()))
		if err != nil {
			continue // the thread ended meanwhile
		}
		if s := strings.TrimSpace(string(data)); s != "" {
			children = append(children, s)
		}
	}
	return strings.Join(children, " ")
}

// TestUnitCacheAbandonedRealRunReaped: with the real ListUnits and a hung
// journalctl mock, runs whose requests all gave up go on, but are killed
// and reaped at the flight timeout, leaving neither child processes nor
// goroutines behind.
func TestUnitCacheAbandonedRealRunReaped(t *testing.T) {
	journaltest.InstallMock(t, journaltest.Scenario{Default: journaltest.Invocation{
		Lines: []string{"a.service", "b.service"}, HoldOpen: true,
	}})
	cache := NewUnitCache(ListUnits, time.Minute)
	cache.flightTimeout = 300 * time.Millisecond
	before := runtime.NumGoroutine()
	for round := range 3 {
		var wg sync.WaitGroup
		for i := range 4 {
			wg.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(100+20*i)*time.Millisecond)
				defer cancel()
				if _, _, err := cache.ListUnits(ctx, 10); !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("round %d: error %v, want context.DeadlineExceeded", round, err)
				}
			})
		}
		wg.Wait()
	}
	deadline := time.Now().Add(5 * time.Second)
	for (runtime.NumGoroutine() > before || childProcesses(t) != "") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if children := childProcesses(t); children != "" {
		t.Fatalf("journalctl children left: %s", children)
	}
	if n := runtime.NumGoroutine(); n > before {
		buf := make([]byte, 1<<16)
		t.Fatalf("goroutines %d -> %d:\n%s", before, n, buf[:runtime.Stack(buf, true)])
	}
}

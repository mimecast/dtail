//go:build linux

package probe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/journal"
	journaltest "github.com/mimecast/dtail/internal/io/journal/testhelper"
)

func journalTarget(t *testing.T) fs.ValidatedReadTarget {
	t.Helper()
	target, err := fs.NewValidatedJournalTarget("journal:ssh.service")
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func TestSampleJournalAsksForTheLastEntries(t *testing.T) {
	mock := journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{Lines: []string{"alpha", "beta", "gamma"}},
	})

	got, err := Sample(context.Background(), journalTarget(t), "journal:ssh.service",
		fs.SampleOptions{})
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if want := (fs.Sample{Lines: []string{"alpha", "beta", "gamma"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("sample = %#v, want %#v", got, want)
	}
	if args := strings.TrimSpace(mock.Args(t)); args != "-u ssh.service -n 500" {
		t.Fatalf("journalctl args = %q, want %q", args, "-u ssh.service -n 500")
	}
}

func TestSampleJournalKeepsTheLastPieces(t *testing.T) {
	journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{Lines: []string{"one", "two", "0123456789"}},
	})

	// The long entry is cut into 4-byte pieces; with 3 lines at most, the
	// last pieces are kept.
	got, err := Sample(context.Background(), journalTarget(t), "journal:ssh.service",
		fs.SampleOptions{MaxLines: 3, MaxLineLength: 4})
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if want := []string{"0123", "4567", "89"}; !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("lines = %q, want %q", got.Lines, want)
	}
}

func TestSampleJournalSkipsAnOversizedNewestEntry(t *testing.T) {
	// 499 entries of 4 bytes (1996 bytes) and a newest one larger than the
	// whole byte limit: the 499 are sampled, the oversized one is skipped.
	lines := make([]string, 0, 500)
	for i := 0; i < 499; i++ {
		lines = append(lines, fmt.Sprintf("%04d", i))
	}
	want := append([]string(nil), lines...)
	lines = append(lines, strings.Repeat("j", 4096))
	journaltest.InstallMock(t, journaltest.Scenario{Default: journaltest.Invocation{Lines: lines}})

	got, err := Sample(context.Background(), journalTarget(t), "journal:ssh.service",
		fs.SampleOptions{WindowBytes: 2048})
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("got %d lines, want the 499 entries before the oversized one", len(got.Lines))
	}
}

// TestSampleJournalScanLinesAndKeep checks a table-targeted journal sample:
// journalctl is asked for ScanLines entries, and only the ones Keep accepts
// are kept, every one of them seen by Observe.
func TestSampleJournalScanLinesAndKeep(t *testing.T) {
	mock := journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{Lines: []string{"keep-1", "drop", "keep-2"}},
	})
	var observed []string
	got, err := Sample(context.Background(), journalTarget(t), "journal:ssh.service", fs.SampleOptions{
		ScanLines: 5000,
		Keep:      func(line []byte) bool { return strings.HasPrefix(string(line), "keep") },
		Observe:   func(line []byte) { observed = append(observed, string(line)) },
	})
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if want := []string{"keep-1", "keep-2"}; !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("lines = %q, want %q", got.Lines, want)
	}
	if len(observed) != 3 {
		t.Fatalf("observed %q, want every entry", observed)
	}
	if args := strings.TrimSpace(mock.Args(t)); args != "-u ssh.service -n 5000" {
		t.Fatalf("journalctl args = %q, want %q", args, "-u ssh.service -n 5000")
	}
}

func TestSampleJournalCanceledWhileRunning(t *testing.T) {
	mock := journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{Lines: []string{"alpha"}, HoldOpen: true},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Sample(ctx, journalTarget(t), "journal:ssh.service", fs.SampleOptions{})
		done <- err
	}()

	// The fake journalctl writes its PID file once it runs, and never exits
	// on its own (HoldOpen): only the cancellation can end the sample.
	waitForFile(t, mock.PIDFile, done)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Sample did not return after its context was canceled")
	}
	// The mock records SIGTERM in its trap handler, which may still run
	// after Sample returned.
	mock.WaitForTerm(t, 10*time.Second)
}

// waitForFile waits until path exists, failing when done delivers first
// (the sample ended before journalctl ran) or after 10 seconds.
func waitForFile(t *testing.T, path string, done <-chan error) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("Sample ended before journalctl ran: %v", err)
		case <-deadline:
			t.Fatalf("journalctl did not start: no %s", path)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestSampleJournalFailures(t *testing.T) {
	t.Run("journalctl missing", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := Sample(context.Background(), journalTarget(t), "journal:ssh.service", fs.SampleOptions{})
		if !errors.Is(err, journal.ErrJournalctlNotFound) {
			t.Fatalf("err = %v, want ErrJournalctlNotFound", err)
		}
	})
	t.Run("journalctl fails", func(t *testing.T) {
		journaltest.InstallMock(t, journaltest.Scenario{
			Default: journaltest.Invocation{ExitCode: 1, Stderr: []string{"no such unit"}},
		})
		if _, err := Sample(context.Background(), journalTarget(t), "journal:ssh.service",
			fs.SampleOptions{}); err == nil {
			t.Fatal("expected an error from a failing journalctl")
		}
	})
}

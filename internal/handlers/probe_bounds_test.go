package handlers

// Tests of the probe's documented bounds and error reasons that need a
// controlled context or sample: the probe timeout (probeTimeout, reported
// as protocol.ProbeErrorTimeout by the sampling and by the path listing), a
// failing log format probe (protocol.ProbeErrorFormat), names that are no
// valid UTF-8 (never offered by a listing) and the cap of the request
// entries looked at (protocol.ProbeMaxFiles).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/mapr/logformat"
	"github.com/mimecast/dtail/internal/protocol"
)

// TestProbeSamplesWithinTheProbeTimeout checks that a probe samples under
// a context with the deadline probeTimeout from its start, so a sample
// that hangs is stopped and the answer still goes out.
func TestProbeSamplesWithinTheProbeTimeout(t *testing.T) {
	file := writeProbeTestFile(t, t.TempDir(), "app.log", probeTestStatsLine("n", "1"))
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	var deadline time.Time
	var hasDeadline bool
	handler.probeSampler = func(ctx context.Context, _ fs.ValidatedReadTarget, _ string,
		_ fs.SampleOptions) (fs.Sample, error) {
		deadline, hasDeadline = ctx.Deadline()
		return fs.Sample{Lines: []string{"x"}}, nil
	}

	before := time.Now()
	answer := handler.newProber().run(context.Background(), protocol.ProbeRequest{ID: "t1", Files: []string{file}})
	after := time.Now()
	if answer.FilesSampled != 1 || len(answer.Errors) != 0 {
		t.Fatalf("answer = %+v, want one file sampled", answer)
	}
	if !hasDeadline || deadline.Before(before.Add(probeTimeout)) || deadline.After(after.Add(probeTimeout)) {
		t.Fatalf("sample context deadline = %v (set %v), want %v after the probe's start",
			deadline, hasDeadline, probeTimeout)
	}
}

// TestProbeTimeoutStopsTheSampling checks what a probe whose context ends
// while it samples answers: the entry being sampled gets
// protocol.ProbeErrorTimeout (not a read failure), and the files after it
// are not sampled but reported by one timeout error of the whole request.
func TestProbeTimeoutStopsTheSampling(t *testing.T) {
	dir := t.TempDir()
	first := writeProbeTestFile(t, dir, "a.log", probeTestStatsLine("n", "1"))
	second := writeProbeTestFile(t, dir, "b.log", probeTestStatsLine("n", "2"))
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var samples atomic.Int32
	handler.probeSampler = func(ctx context.Context, _ fs.ValidatedReadTarget, _ string,
		_ fs.SampleOptions) (fs.Sample, error) {
		samples.Add(1)
		cancel() // the probe's time runs out during this sample
		<-ctx.Done()
		return fs.Sample{}, fmt.Errorf("read sample: %w", ctx.Err())
	}

	answer := handler.newProber().run(ctx, protocol.ProbeRequest{ID: "t1", Files: []string{first, second}})
	want := []protocol.ProbeError{
		{File: first, Error: protocol.ProbeErrorTimeout},
		{File: "", Error: protocol.ProbeErrorTimeout},
	}
	if !slices.Equal(answer.Errors, want) || answer.FilesSampled != 0 || samples.Load() != 1 {
		t.Fatalf("answer = %+v after %d samples, want errors %+v after one sample",
			answer, samples.Load(), want)
	}
}

// TestProbeSampleErrorWithoutTimeout checks the negative case: a sample
// that fails while the probe's time has not run out is a read failure of
// its entry, even when the sampler's error is a context error of its own,
// and the probe goes on with the next file.
func TestProbeSampleErrorWithoutTimeout(t *testing.T) {
	dir := t.TempDir()
	first := writeProbeTestFile(t, dir, "a.log", probeTestStatsLine("n", "1"))
	second := writeProbeTestFile(t, dir, "b.log", probeTestStatsLine("n", "2"))
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	handler.probeSampler = func(ctx context.Context, _ fs.ValidatedReadTarget, path string,
		_ fs.SampleOptions) (fs.Sample, error) {
		if path == first {
			return fs.Sample{}, fmt.Errorf("read sample: %w", context.DeadlineExceeded)
		}
		return fs.Sample{Lines: []string{"x"}}, nil
	}

	answer := handler.newProber().run(context.Background(),
		protocol.ProbeRequest{ID: "t1", Files: []string{first, second}})
	want := []protocol.ProbeError{{File: first, Error: protocol.CommandFailureReadingFile}}
	if !slices.Equal(answer.Errors, want) || answer.FilesSampled != 1 {
		t.Fatalf("answer = %+v, want errors %+v and the second file sampled", answer, want)
	}
}

// TestProbeListTimeoutWhileChecking checks that a journal listing whose
// probe context ends after the units were read, or while they are read,
// reports protocol.ProbeErrorTimeout (and Truncated once names were
// left unchecked), not an empty but complete listing or a failed
// journalctl run.
func TestProbeListTimeoutWhileChecking(t *testing.T) {
	handler := newProbeTestHandler(t, []string{`readfiles:^journal:app-.*\.service$`}, nil,
		protocol.CapabilityJournalV1)
	tests := map[string]func(cancel context.CancelFunc) ([]string, bool, error){
		"after listing the units": func(cancel context.CancelFunc) ([]string, bool, error) {
			cancel()
			return []string{"app-db.service", "app-web.service"}, false, nil
		},
		"while listing the units": func(cancel context.CancelFunc) ([]string, bool, error) {
			cancel()
			return nil, false, fmt.Errorf("journalctl: %w", context.Canceled)
		},
	}
	for name, lister := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			handler.probeUnitLister = func(context.Context, int) ([]string, bool, error) {
				return lister(cancel)
			}
			answer := handler.newProber().runList(ctx, protocol.ProbeRequest{ID: "l1", List: "journal:"})
			want := []protocol.ProbeError{{Error: protocol.ProbeErrorTimeout}}
			if len(answer.Entries) != 0 || !slices.Equal(answer.Errors, want) {
				t.Fatalf("listing = %+v, want no entries and %+v", answer, want)
			}
			if name == "after listing the units" && !answer.Truncated {
				t.Fatalf("listing = %+v, want truncated", answer)
			}
		})
	}
}

// probeTestFailingFormat is the name of a log format registered for the
// tests whose probe fails for a sample with probeTestFailingLine and finds
// nothing in any other sample, so other tests do not see it.
const (
	probeTestFailingFormat = "probetest-failing"
	probeTestFailingLine   = "PROBETEST-FAIL-THE-FORMAT-PROBE"
)

var registerFailingFormat sync.Once

// failingFormatParser is the parser of probeTestFailingFormat: it parses no
// line, and its probe fails for a sample with probeTestFailingLine.
type failingFormatParser struct{}

func (failingFormatParser) MakeFields(string, string) (map[string]string, error) {
	return nil, logformat.ErrIgnoreFields
}

func (failingFormatParser) Probe(sample logformat.ProbeSample) (logformat.ProbeResult, error) {
	for _, line := range sample.Lines {
		if strings.Contains(line.Line, probeTestFailingLine) {
			return logformat.ProbeResult{}, errors.New("probe failed on purpose")
		}
	}
	return logformat.ProbeResult{}, nil
}

// TestProbeReportsFailingFormatProbes checks that a log format whose probe
// fails is reported by name with protocol.ProbeErrorFormat, while the other
// formats are still answered, and that it is not reported otherwise.
func TestProbeReportsFailingFormatProbes(t *testing.T) {
	registerFailingFormat.Do(func() {
		err := logformat.RegisterParser(probeTestFailingFormat,
			func(string, string, int) (logformat.Parser, error) { return failingFormatParser{}, nil })
		if err != nil {
			t.Fatalf("register %s: %v", probeTestFailingFormat, err)
		}
	})
	dir := t.TempDir()
	failing := writeProbeTestFile(t, dir, "failing.log", probeTestStatsLine("marker", probeTestFailingLine))
	plain := writeProbeTestFile(t, dir, "plain.log", probeTestStatsLine("marker", "1"))
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)

	answer, _ := probeFiles(t, handler, failing)
	want := []protocol.ProbeError{{File: probeTestFailingFormat, Error: protocol.ProbeErrorFormat}}
	if !slices.Equal(answer.Errors, want) || answer.FilesSampled != 1 {
		t.Fatalf("answer = %+v, want errors %+v", answer, want)
	}
	if _, ok := answer.Formats["default"].Tables["STATS"]["marker"]; !ok {
		t.Fatalf("answer = %+v, want the default format's fields despite the failing format", answer)
	}
	if answer, _ := probeFiles(t, handler, plain); len(answer.Errors) != 0 {
		t.Fatalf("answer without the failing line = %+v, want no errors", answer)
	}
}

// TestProbeListSkipsNamesThatAreNoUTF8 checks that a listing never offers a
// file, directory or journal unit whose name is no valid UTF-8, although
// the permissions allow it.
func TestProbeListSkipsNamesThatAreNoUTF8(t *testing.T) {
	dir := realTempDir(t)
	writeProbeTestFile(t, dir, "good.log", "line")
	if err := os.WriteFile(filepath.Join(dir, "bad\xff.log"), []byte("line\n"), 0o644); err != nil {
		t.Skipf("file system refuses a name that is no UTF-8: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dir\xfe"), 0o755); err != nil {
		t.Fatalf("create directory: %v", err)
	}
	handler := newProbeTestHandler(t, []string{"readfiles:^" + regexp.QuoteMeta(dir) + "/",
		`readfiles:^journal:`}, nil, protocol.CapabilityJournalV1)
	handler.probeUnitLister = func(context.Context, int) ([]string, bool, error) {
		return []string{"bad\xff.service", "app.service"}, false, nil
	}

	for prefix, want := range map[string][]string{
		dir + "/":    {"good.log"},
		dir + "/b":   {},
		dir + "/d":   {},
		"journal:":   {"app.service"},
		"journal:ba": {},
	} {
		answer := probeList(t, handler, prefix)
		if got := entryNames(answer); !slices.Equal(got, want) || answer.Truncated || len(answer.Errors) != 0 {
			t.Errorf("listing of %q = %q (%+v), want %q", prefix, got, answer, want)
		}
	}

	// A unit name that is no UTF-8 is no valid journal target either, so
	// the permission check would refuse it too; it must not even take one
	// of the listing's checks (probeListLimits.maxChecks) from a valid name.
	handler.probeListLimits = probeListLimits{maxChecks: 1}
	handler.probeUnitLister = func(context.Context, int) ([]string, bool, error) {
		return []string{"a\xff.service", "b.service"}, false, nil
	}
	answer := probeList(t, handler, "journal:")
	if got := entryNames(answer); !slices.Equal(got, []string{"b.service"}) || answer.Truncated {
		t.Errorf("listing with one check = %q (%+v), want [b.service], complete", got, answer)
	}
}

// TestProbeLooksAtNoMoreEntriesThanItSamples checks that a request of more
// than protocol.ProbeMaxFiles entries has only its first ones looked at,
// also when they match no file (so the later cap of the targets is never
// reached): the others are neither globbed nor reported.
func TestProbeLooksAtNoMoreEntriesThanItSamples(t *testing.T) {
	dir := t.TempDir()
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	handler.probeSampler = failingProbeSampler(t)
	var entries []string
	for i := range protocol.ProbeMaxFiles + 4 {
		entries = append(entries, filepath.Join(dir, fmt.Sprintf("missing%02d.log", i)))
	}

	answer, _ := probeFiles(t, handler, entries...)
	want := []protocol.ProbeError{{Error: protocol.ProbeErrorTooManyFiles}}
	for _, entry := range entries[:protocol.ProbeMaxFiles] {
		want = append(want, protocol.ProbeError{File: entry, Error: protocol.CommandFailureNoFile})
	}
	if !slices.Equal(answer.Errors, want) || answer.FilesSampled != 0 {
		t.Fatalf("answer errors = %+v, want %+v", answer.Errors, want)
	}

	// Negative: a request of exactly protocol.ProbeMaxFiles entries is not
	// cut.
	answer, _ = probeFiles(t, handler, entries[:protocol.ProbeMaxFiles]...)
	if !slices.Equal(answer.Errors, want[1:]) {
		t.Fatalf("answer errors = %+v, want %+v", answer.Errors, want[1:])
	}
}

package aggregate

import (
	"bytes"
	"fmt"
	"maps"
	"sync"
	"testing"

	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/logformat"
)

const csvSourceTestQuery = `select count($line) group by label logformat csv`

// groupSamples returns the sample count of every group aggregated so far.
func groupSamples(aggregate *Aggregate) map[string]int {
	aggregate.serializer.groupMu.Lock()
	defer aggregate.serializer.groupMu.Unlock()
	samples := make(map[string]int, len(aggregate.serializer.groupSets))
	for group, set := range aggregate.serializer.groupSets {
		samples[group] = set.Samples
	}
	return samples
}

func newCSVSourceTestAggregate(t *testing.T) *Aggregate {
	t.Helper()
	aggregate, err := newAggregateFromTextForTest(csvSourceTestQuery, logging.NopLogger{})
	if err != nil {
		t.Fatalf("Failed to create aggregate: %v", err)
	}
	return aggregate
}

func flushProcessor(t *testing.T, processor *Processor) {
	t.Helper()
	if err := processor.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
}

func closeProcessor(t *testing.T, processor *Processor) {
	t.Helper()
	if err := processor.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// csvRows returns a header row followed by n data rows labelled l0 to l3.
func csvRows(header string, n int) []string {
	rows := make([]string, 0, n+1)
	rows = append(rows, header)
	for i := range n {
		rows = append(rows, fmt.Sprintf("l%d,%d", i%4, i))
	}
	return rows
}

// TestCSVProcessorsSharingGlobIDKeepTheirOwnHeader forces data rows of one
// file to reach the parser before the header row of another file with the
// same glob ID. Readers pass the glob ID as sourceID, and it is the same for
// files with the same base name in different directories and for the reads
// before and after a follow-mode reader reopens a rotated file, which it does
// with a new Processor. (An in-place truncation keeps the same Processor, see
// TestProcessorSourceRestartedDrainsThenTakesFreshKey and
// TestCSVFollowReaderInPlaceTruncationInstallsNewHeader.) Parsed
// under that shared sourceID, the second file's header row was mapped as data
// (an extra "label" group), or, with a different column order, every row of
// it was mapped against the other file's header.
func TestCSVProcessorsSharingGlobIDKeepTheirOwnHeader(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, aggregate *Aggregate)
		want map[string]int
	}{
		{
			name: "other file's data rows parsed first",
			run: func(t *testing.T, aggregate *Aggregate) {
				first := NewProcessor(aggregate, "x.csv")
				second := NewProcessor(aggregate, "x.csv")
				feedLines(t, second, "label,value", "b,3")
				flushProcessor(t, second)
				feedLines(t, first, "label,value", "a,1", "a,2")
				closeProcessor(t, first)
				closeProcessor(t, second)
			},
			want: map[string]int{"a": 2, "b": 1},
		},
		{
			name: "other file's full batch parsed first",
			run: func(t *testing.T, aggregate *Aggregate) {
				first := NewProcessor(aggregate, "x.csv")
				second := NewProcessor(aggregate, "x.csv")
				// One header and processorBatchSize-1 rows: the batch
				// is full and drained inline by ProcessLine.
				feedLines(t, second, csvRows("label,value", processorBatchSize-1)...)
				feedLines(t, first, "label,value", "a,1")
				closeProcessor(t, first)
				closeProcessor(t, second)
			},
			want: map[string]int{"a": 1, "l0": 25, "l1": 25, "l2": 25, "l3": 24},
		},
		{
			name: "different column order",
			run: func(t *testing.T, aggregate *Aggregate) {
				first := NewProcessor(aggregate, "x.csv")
				second := NewProcessor(aggregate, "x.csv")
				feedLines(t, second, "value,label", "3,b")
				flushProcessor(t, second)
				feedLines(t, first, "label,value", "a,1", "a,2")
				closeProcessor(t, first)
				closeProcessor(t, second)
			},
			want: map[string]int{"a": 2, "b": 1},
		},
		{
			name: "file reopened after rotation",
			run: func(t *testing.T, aggregate *Aggregate) {
				before := NewProcessor(aggregate, "x.csv")
				feedLines(t, before, "label,value", "a,1", "a,2")
				closeProcessor(t, before)
				after := NewProcessor(aggregate, "x.csv")
				feedLines(t, after, "value,label", "3,c")
				closeProcessor(t, after)
			},
			want: map[string]int{"a": 2, "c": 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			aggregate := newCSVSourceTestAggregate(t)
			tt.run(t, aggregate)
			if got := groupSamples(aggregate); !maps.Equal(got, tt.want) {
				t.Errorf("groups = %v, want %v", got, tt.want)
			}
			if got := aggregate.errors.Load(); got != 0 {
				t.Errorf("errors = %d, want 0", got)
			}
		})
	}
}

// TestCSVConcurrentProcessorsExactCounts runs the multi-file shape of a CSV
// dmap query: one processor per file, each fed by its own goroutine, half of
// them sharing a glob ID. Every file's header must be recognized and every
// data row counted exactly once.
func TestCSVConcurrentProcessorsExactCounts(t *testing.T) {
	const files = 8
	const rowsPerFile = 3000

	aggregate := newCSVSourceTestAggregate(t)
	lines := csvRows("label,value", rowsPerFile)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for file := range files {
		globID := fmt.Sprintf("f%d.csv", file)
		if file%2 == 0 {
			globID = "dup.csv"
		}
		processor := NewProcessor(aggregate, globID)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i, line := range lines {
				if err := processor.ProcessLine(bytes.NewBufferString(line), uint64(i+1), globID); err != nil {
					t.Errorf("ProcessLine() error = %v", err)
					return
				}
			}
			if err := processor.Close(); err != nil {
				t.Errorf("Close() error = %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	perGroup := files * rowsPerFile / 4
	want := map[string]int{"l0": perGroup, "l1": perGroup, "l2": perGroup, "l3": perGroup}
	if got := groupSamples(aggregate); !maps.Equal(got, want) {
		t.Errorf("groups = %v, want %v", got, want)
	}
	if got := aggregate.errors.Load(); got != 0 {
		t.Errorf("errors = %d, want 0", got)
	}
	if got, want := aggregate.linesProcessed.Load(), uint64(files*(rowsPerFile+1)); got != want {
		t.Errorf("linesProcessed = %d, want %d", got, want)
	}
}

// sourceRecordingParser records the sourceIDs lines are parsed under and the
// sourceIDs released, in call order.
type sourceRecordingParser struct {
	mu     sync.Mutex
	events []string
}

var _ logformat.Parser = (*sourceRecordingParser)(nil)
var _ logformat.SourceReleaser = (*sourceRecordingParser)(nil)

func (p *sourceRecordingParser) MakeFields(_, sourceID string) (map[string]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, "parse "+sourceID)
	return map[string]string{"label": "x"}, nil
}

func (p *sourceRecordingParser) ReleaseSource(sourceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, "release "+sourceID)
}

// TestProcessorParsesUnderOwnSourceKeyAndReleasesIt pins the contract the CSV
// header install depends on: every processor parses under a source key of its
// own, whatever sourceID the reader passes, and releases that key only after
// its last line was parsed.
func TestProcessorParsesUnderOwnSourceKeyAndReleasesIt(t *testing.T) {
	query, err := mapr.NewQuery(csvSourceTestQuery, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	parser := &sourceRecordingParser{}
	aggregate, err := New(query, parser, "aggregate-test", logging.NopLogger{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	first := NewProcessor(aggregate, "x.csv")
	second := NewProcessor(aggregate, "x.csv")
	if first.sourceKey == second.sourceKey {
		t.Fatalf("processors share source key %q", first.sourceKey)
	}
	feedLines(t, first, "one", "two")
	closeProcessor(t, first)
	feedLines(t, second, "three")
	closeProcessor(t, second)
	// A second Close must not release again.
	closeProcessor(t, second)

	want := []string{
		"parse " + first.sourceKey,
		"parse " + first.sourceKey,
		"release " + first.sourceKey,
		"parse " + second.sourceKey,
		"release " + second.sourceKey,
	}
	parser.mu.Lock()
	defer parser.mu.Unlock()
	if fmt.Sprint(parser.events) != fmt.Sprint(want) {
		t.Errorf("parser events = %q, want %q", parser.events, want)
	}
}

var _ line.SourceRestarter = (*Processor)(nil)

// TestProcessorSourceRestartedDrainsThenTakesFreshKey pins what an in-place
// truncation does to a processor: the lines batched before it (not yet
// flushed) are parsed under the old key, the old key is released, and every
// later line is parsed under a new key of the same processor, which Close
// releases in turn.
func TestProcessorSourceRestartedDrainsThenTakesFreshKey(t *testing.T) {
	query, err := mapr.NewQuery(csvSourceTestQuery, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	parser := &sourceRecordingParser{}
	aggregate, err := New(query, parser, "aggregate-test", logging.NopLogger{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	processor := NewProcessor(aggregate, "x.csv")
	oldKey := processor.sourceKey
	feedLines(t, processor, "one", "two")
	processor.SourceRestarted()
	newKey := processor.sourceKey
	if newKey == oldKey {
		t.Fatalf("source key %q unchanged by SourceRestarted", oldKey)
	}
	feedLines(t, processor, "three")
	closeProcessor(t, processor)

	want := []string{
		"parse " + oldKey,
		"parse " + oldKey,
		"release " + oldKey,
		"parse " + newKey,
		"release " + newKey,
	}
	parser.mu.Lock()
	defer parser.mu.Unlock()
	if fmt.Sprint(parser.events) != fmt.Sprint(want) {
		t.Errorf("parser events = %q, want %q", parser.events, want)
	}
}

// TestCSVProcessorSourceRestartedInstallsNewHeader is the CSV view of the
// same contract: after SourceRestarted the processor's next line is a header
// again, so rewritten content with a different column order is mapped
// against its own header.
func TestCSVProcessorSourceRestartedInstallsNewHeader(t *testing.T) {
	aggregate := newCSVSourceTestAggregate(t)
	processor := NewProcessor(aggregate, "x.csv")
	feedLines(t, processor, "label,value", "a,1", "a,2")
	processor.SourceRestarted()
	feedLines(t, processor, "value,label", "3,z", "4,z")
	closeProcessor(t, processor)

	want := map[string]int{"a": 2, "z": 2}
	if got := groupSamples(aggregate); !maps.Equal(got, want) {
		t.Errorf("groups = %v, want %v", got, want)
	}
	if got := aggregate.errors.Load(); got != 0 {
		t.Errorf("errors = %d, want 0", got)
	}
}

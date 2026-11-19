package aggregate

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/logformat"
)

// reusingMapParser implements only logformat.Parser and clears and refills one
// map for every line, which Parser.MakeFields allows.
type reusingMapParser struct {
	fields map[string]string
}

func (p *reusingMapParser) MakeFields(maprLine, _ string) (map[string]string, error) {
	clear(p.fields)
	for _, token := range strings.Split(maprLine, "|") {
		key, value, ok := strings.Cut(token, "=")
		if !ok {
			return nil, logformat.ErrIgnoreFields
		}
		p.fields[key] = value
	}
	return p.fields, nil
}

// panickingParser panics on its panicAt-th call (counting from 1).
type panickingParser struct {
	calls   int
	panicAt int
}

func (p *panickingParser) MakeFields(maprLine, _ string) (map[string]string, error) {
	p.calls++
	if p.calls == p.panicAt {
		panic("parser boom")
	}
	return map[string]string{"host": maprLine}, nil
}

func newAggregateWithParser(t *testing.T, queryText string,
	parser logformat.Parser) *Aggregate {

	t.Helper()
	query, err := mapr.NewQuery(queryText, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	aggregate, err := New(query, parser, "aggregate-test", logging.NopLogger{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return aggregate
}

// TestProcessorCopiesFieldsOfMapReusingParser pins that a batch's merge phase
// does not read a map the parser has since reused for a later line of the
// same batch. Without the copy every line of the batch is merged with the
// fields of the batch's last line: alpha would get last(color)=purple.
func TestProcessorCopiesFieldsOfMapReusingParser(t *testing.T) {
	parser := &reusingMapParser{fields: make(map[string]string)}
	aggregate := newAggregateWithParser(t,
		`from STATS select count(host),last(color) group by host`, parser)
	processor := NewProcessor(aggregate, "reuse")
	feedLines(t, processor,
		"host=alpha|color=orange",
		"host=alpha|color=violet",
		"host=beta|color=purple",
	)
	if err := processor.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	lastColor := aggregate.query.Select[1].FieldStorage
	aggregate.serializer.groupMu.Lock()
	defer aggregate.serializer.groupMu.Unlock()
	for group, want := range map[string]struct {
		samples int
		color   string
	}{
		"alpha": {2, "violet"},
		"beta":  {1, "purple"},
	} {
		set, ok := aggregate.serializer.groupSets[group]
		if !ok {
			t.Fatalf("group %q missing; groups: %v", group, aggregate.serializer.groupSets)
		}
		if set.Samples != want.samples {
			t.Errorf("group %q has %d samples, want %d", group, set.Samples, want.samples)
		}
		if got := set.SValues[lastColor]; got != want.color {
			t.Errorf("group %q last(color) = %q, want %q", group, got, want.color)
		}
	}
}

// TestProcessRawBatchKeepsParserPanicValue pins that a parser panic part way
// through a batch of several lines reaches the caller unchanged. Recycling the
// batch scratch must clear only the line scratches that exist, instead of
// slicing them by the batch length and replacing the panic with a runtime
// error of its own.
func TestProcessRawBatchKeepsParserPanicValue(t *testing.T) {
	parser := &panickingParser{panicAt: 4}
	aggregate := newAggregateWithParser(t,
		`from STATS select count(host) group by host`, parser)

	batch := make([]rawLine, 5)
	for i := range batch {
		batch[i] = rawLine{content: bytes.NewBufferString("alpha"), sourceID: "panic"}
	}
	recovered := func() (recovered any) {
		defer func() { recovered = recover() }()
		// A fresh batch scratch has fewer line scratches than the batch has
		// lines, which is the case that used to fail. A pooled one could
		// already hold a full batch of them and let the test pass vacuously.
		aggregate.processRawBatchWith(&batchScratch{}, batch)
		return nil
	}()
	if recovered != "parser boom" {
		t.Fatalf("recovered %#v, want the parser's own panic value \"parser boom\"",
			recovered)
	}
}

// TestBatchScratchRecycleClearsOnlyUsedScratches covers the used-scratch
// bookkeeping deterministically with a fresh batch scratch.
func TestBatchScratchRecycleClearsOnlyUsedScratches(t *testing.T) {
	scratch := &batchScratch{}
	for i := 0; i < 3; i++ {
		line := scratch.line(i)
		line.fields["host"] = "alpha"
		line.key = append(line.key, "alpha"...)
	}
	if scratch.used != 3 {
		t.Fatalf("used = %d after three lines, want 3", scratch.used)
	}
	scratch.clear()
	if scratch.used != 0 {
		t.Errorf("used = %d after clear, want 0", scratch.used)
	}
	for i, line := range scratch.lines {
		if len(line.fields) != 0 || len(line.key) != 0 {
			t.Errorf("line scratch %d kept borrowed views after clear", i)
		}
	}
}

// fillBatch simulates a full batch of identical lines with the given number of
// fields and group-key length and clears the batch scratch afterwards.
func fillBatch(scratch *batchScratch, fields, keyLen int) {
	for i := 0; i < processorBatchSize; i++ {
		fillLineScratch(scratch.line(i), fields, keyLen)
	}
	scratch.clear()
}

// retainedIdle sums the idle headroom that the line scratches of scratch
// retain, which is what the batch budget charges.
func retainedIdle(scratch *batchScratch) (fields, keyBytes int) {
	for _, line := range scratch.lines {
		fields += scratch.idleFields(line)
		keyBytes += scratch.idleKeyBytes(line)
	}
	return fields, keyBytes
}

// fillLineScratch simulates parseLine for one line with the given number of
// fields and a group key of keyLen bytes.
func fillLineScratch(line *lineScratch, fields, keyLen int) {
	for f := 0; f < fields; f++ {
		line.fields[fieldName(f)] = "v"
	}
	if n := len(line.fields); n > line.maxFields {
		line.maxFields = n
	}
	// One append of the whole key, as buildGroupKey does for a single group
	// field, so the buffer grows to the same capacity as in production.
	line.key = append(line.key[:0], keyBytes[:keyLen]...)
}

var keyBytes = make([]byte, maxRetainedScratchKeyBytes)

// fieldNames are precomputed so that filling a scratch in an allocation test
// does not allocate the names themselves.
var fieldNames = func() []string {
	names := make([]string, maxRetainedScratchFields)
	for f := range names {
		names[f] = string(rune('a'+f%26)) + strings.Repeat("x", f/26)
	}
	return names
}()

func fieldName(f int) string { return fieldNames[f] }

// TestBatchScratchRetentionBudget pins the batch-level retention limits: line
// scratches that each stay below the per-line limits must not add up to
// processorBatchSize times those limits of idle storage in a pooled batch
// scratch. Storage up to the largest line of the last retentionHistory batches
// is kept; only idle headroom beyond it is charged, and the scratches with the
// largest headroom are the ones shrunk, to the size of that largest line.
func TestBatchScratchRetentionBudget(t *testing.T) {
	const ordinaryFields, ordinaryKeyLen = 30, 5
	scratch := &batchScratch{}
	for i := 0; i < processorBatchSize; i++ {
		fillLineScratch(scratch.line(i), maxRetainedScratchFields, maxRetainedScratchKeyBytes)
	}
	mapIDs := make([]uintptr, 0, processorBatchSize)
	keyIDs := make([]uintptr, 0, processorBatchSize)
	for _, line := range scratch.lines {
		mapIDs = append(mapIDs, mapIdentity(line.fields))
		keyIDs = append(keyIDs, sliceIdentity(line.key))
	}
	scratch.clear()
	// Everything the batch used is kept, however large: it is bounded by the
	// per-line limits, and the next batch of the same lines reuses it.
	for i, line := range scratch.lines {
		if mapIdentity(line.fields) != mapIDs[i] || sliceIdentity(line.key) != keyIDs[i] {
			t.Fatalf("line scratch %d lost storage its line used in the batch just processed", i)
		}
	}

	// The next retentionHistory-1 ordinary batches keep that storage: the
	// outlier batch is still in the history, so nothing is idle yet.
	for b := 1; b < retentionHistory; b++ {
		fillBatch(scratch, ordinaryFields, ordinaryKeyLen)
		for i, line := range scratch.lines {
			if mapIdentity(line.fields) != mapIDs[i] || sliceIdentity(line.key) != keyIDs[i] {
				t.Fatalf("ordinary batch %d shrank line scratch %d although a "+
					"batch of the history used its storage", b, i)
			}
		}
	}
	// The retentionHistory-th ordinary batch drops the outlier batch from the
	// history, which leaves its storage idle, and it is trimmed.
	fillBatch(scratch, ordinaryFields, ordinaryKeyLen)
	fields, keyBytes := retainedIdle(scratch)
	if fields > maxRetainedBatchScratchFields {
		t.Errorf("batch scratch retains %d idle fields, want at most %d",
			fields, maxRetainedBatchScratchFields)
	}
	if keyBytes > maxRetainedBatchScratchKeyBytes {
		t.Errorf("batch scratch retains %d idle key bytes, want at most %d",
			keyBytes, maxRetainedBatchScratchKeyBytes)
	}
	// The budget is not shrunk further than it has to be: it holds eight
	// scratches of idle headroom near the per-line fields limit.
	perLineFields := maxRetainedScratchFields - ordinaryFields
	if want := maxRetainedBatchScratchFields / perLineFields * perLineFields; fields != want {
		t.Errorf("batch scratch retains %d idle fields, want %d", fields, want)
	}
	for i, line := range scratch.lines {
		if scratch.idleFields(line) == 0 && (len(line.fields) != 0 || line.maxFields != ordinaryFields) {
			t.Errorf("shrunk line scratch %d has %d fields and peak %d, want 0 and %d",
				i, len(line.fields), line.maxFields, ordinaryFields)
		}
		if scratch.idleKeyBytes(line) == 0 && cap(line.key) != scratchKeyCapacity {
			t.Errorf("shrunk line scratch %d has key capacity %d, want %d",
				i, cap(line.key), scratchKeyCapacity)
		}
	}

	// A shrunk scratch keeps room for the largest line of the recent
	// batches, not just for its own line, so any line of the next batch up to
	// that size fits it without growing it again.
	varying := &batchScratch{}
	fillBatch(varying, ordinaryFields, maxRetainedScratchKeyBytes)
	for b := 0; b < retentionHistory; b++ {
		fillLineScratch(varying.line(0), ordinaryFields, 4096)
		for i := 1; i < processorBatchSize; i++ {
			fillLineScratch(varying.line(i), ordinaryFields, ordinaryKeyLen)
		}
		varying.clear()
	}
	for i, line := range varying.lines {
		if c := cap(line.key); c != maxRetainedScratchKeyBytes && c != 4096 {
			t.Fatalf("line scratch %d has key capacity %d, want %d (kept) or "+
				"%d (shrunk to the longest key of the batch)",
				i, c, maxRetainedScratchKeyBytes, 4096)
		}
	}

	// A scratch whose storage stays within the budget keeps it, so a steady
	// line of that size fits it without growing it again.
	large := &batchScratch{}
	fillLineScratch(large.line(0), 2*scratchFieldsCapacity, 64*1024)
	large.clear()
	fillLineScratch(large.line(0), 2*scratchFieldsCapacity, 16*1024)
	large.clear() // 48 KiB of idle key headroom is within budget: kept
	if cap(large.lines[0].key) < 64*1024 {
		t.Fatalf("key buffer within budget was shrunk to %d", cap(large.lines[0].key))
	}

	// Over budget, the largest idle scratches go first, wherever they sit: a
	// moderately oversized early scratch survives larger later ones.
	mixed := &batchScratch{}
	fillLineScratch(mixed.line(0), 200, 4096)
	for i := 1; i < processorBatchSize; i++ {
		fillLineScratch(mixed.line(i), maxRetainedScratchFields, maxRetainedScratchKeyBytes)
	}
	mixed.clear()
	early := mixed.lines[0]
	earlyMap, earlyKey := mapIdentity(early.fields), sliceIdentity(early.key)
	for b := 0; b < retentionHistory; b++ {
		fillBatch(mixed, ordinaryFields, ordinaryKeyLen)
	}
	if mapIdentity(early.fields) != earlyMap || early.maxFields != 200 {
		t.Errorf("smaller early line scratch lost its map to larger later ones")
	}
	if sliceIdentity(early.key) != earlyKey {
		t.Errorf("smaller early line scratch lost its key buffer to larger later ones")
	}

	// An ordinary full batch stays within the budget and is reused as is.
	ordinary := &batchScratch{}
	for i := 0; i < processorBatchSize; i++ {
		fillLineScratch(ordinary.line(i), 30, 5)
	}
	ids := make([]uintptr, 0, processorBatchSize)
	for _, line := range ordinary.lines {
		ids = append(ids, mapIdentity(line.fields))
	}
	ordinary.clear()
	for i, line := range ordinary.lines {
		if mapIdentity(line.fields) != ids[i] {
			t.Fatalf("ordinary line scratch %d lost its map to the batch budget", i)
		}
	}
}

// TestBatchScratchRecoversFromOutlierBatch is the regression test for a
// budget that charged default-sized storage and admitted scratches in order:
// after one outlier batch, the oversized early scratches used up the budget
// and every later default-sized scratch was replaced on every batch. After an
// outlier batch, the next retentionHistory-1 ordinary batches keep the outlier
// storage and must not allocate; the retentionHistory-th is the recovery batch,
// which trims what stays idle to the budget; and ordinary batches after it
// must not allocate either. A scratch shrunk in the recovery batch keeps room
// for the largest ordinary line of the recent batches, so ordinary lines
// larger than the default do not have to grow it again in the batch after.
func TestBatchScratchRecoversFromOutlierBatch(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under the race detector")
	}
	for _, tc := range []struct {
		name              string
		outliers          int
		outlierFields     int
		outlierKeyLength  int
		ordinaryFields    int
		ordinaryKeyLength int
	}{
		// 4 group keys of about 60 KiB and 8 lines of 1000 fields each fit
		// the budget exactly as the reported cases; 20 outliers exceed it.
		{"keys", 4, 5, 60 * 1024, 20, 64},
		{"fields", 8, 1000, 5, 20, 64},
		{"keys over budget", 20, 5, 60 * 1024, 20, 64},
		{"fields over budget", 20, 1000, 5, 20, 64},
		// Ordinary lines above the default size: shrinking to the default
		// instead of to what they used would make them regrow once more.
		{"keys over budget, large ordinary", 20, 5, 60 * 1024, 20, 8 * 1024},
		{"fields over budget, large ordinary", 20, 1000, 5, 100, 64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var scratch *batchScratch
			batch := func(outliers int) {
				for i := 0; i < processorBatchSize; i++ {
					if i < outliers {
						fillLineScratch(scratch.line(i), tc.outlierFields, tc.outlierKeyLength)
						continue
					}
					fillLineScratch(scratch.line(i), tc.ordinaryFields, tc.ordinaryKeyLength)
				}
				scratch.clear()
			}
			// Every attempt replays the whole sequence on a fresh scratch, so
			// each measured batch sits at the same point of it every time.
			requireAllocationFreeAttempt(t, func() (failures []string) {
				scratch = &batchScratch{}
				batch(tc.outliers)
				for b := 1; b < retentionHistory; b++ {
					if allocs := allocsOfOneRun(func() { batch(0) }); allocs != 0 {
						failures = append(failures, fmt.Sprintf("ordinary batch %d "+
							"after the outlier batch, before the recovery batch, made "+
							"%d allocations, want 0", b, allocs))
					}
				}
				batch(0) // the recovery batch
				// testing.AllocsPerRun warms up with one unmeasured run, which
				// would hide a batch that regrows what the recovery batch
				// shrank, so the first batch after recovery is measured on its
				// own.
				if allocs := allocsOfOneRun(func() { batch(0) }); allocs != 0 {
					failures = append(failures, fmt.Sprintf("the first ordinary "+
						"batch after the recovery batch made %d allocations, want 0",
						allocs))
				}
				return failures
			})

			fields, keyBytes := retainedIdle(scratch)
			if fields > maxRetainedBatchScratchFields || keyBytes > maxRetainedBatchScratchKeyBytes {
				t.Errorf("batch scratch retains %d idle fields and %d idle key "+
					"bytes, want at most %d and %d", fields, keyBytes,
					maxRetainedBatchScratchFields, maxRetainedBatchScratchKeyBytes)
			}
			if allocs := testing.AllocsPerRun(20, func() { batch(0) }); allocs != 0 {
				t.Errorf("an ordinary batch after an outlier batch made %.1f "+
					"allocations, want 0", allocs)
			}
		})
	}
}

// TestBatchScratchVaryingLineSizesAllocationFree is the unit-level companion
// of TestProcessorVaryingKeyLengthsAllocationFree, for both group-key length
// and field count: every line draws its size uniformly from zero to a
// maximum, so a scratch's own last line says little about its next one. A
// budget measuring idle headroom against each scratch's own last line counted
// batch; against the largest line of the recent batches, the batches of such a
// batch; against the largest line of the batch, the batches of such a
// workload reuse all scratches once warmed up.
func TestBatchScratchVaryingLineSizesAllocationFree(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under the race detector")
	}
	for _, tc := range []struct {
		name      string
		maxFields int
		maxKey    int
	}{
		{"keys 8 KiB", 5, 8 * 1024},
		{"keys 16 KiB", 5, 16 * 1024},
		{"fields 300", 300, 5},
		{"fields 1000", 1000, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A fixed seed keeps the line sizes, and with them the
			// allocation count, the same on every run.
			rng := rand.New(rand.NewPCG(uint64(tc.maxFields), uint64(tc.maxKey)))
			scratch := &batchScratch{}
			batch := func() {
				for i := 0; i < processorBatchSize; i++ {
					fillLineScratch(scratch.line(i), rng.IntN(tc.maxFields+1), rng.IntN(tc.maxKey+1))
				}
				scratch.clear()
			}
			// Warm-up: every scratch grows to the largest sizes it sees.
			for i := 0; i < 100; i++ {
				batch()
			}
			if allocs := testing.AllocsPerRun(100, batch); allocs != 0 {
				t.Errorf("a batch of lines with up to %d fields and %d key "+
					"bytes made %.2f allocations, want 0", tc.maxFields, tc.maxKey, allocs)
			}
		})
	}
}

// allocsOfOneRun returns the number of heap allocations of a single call of
// f, without the unmeasured warm-up run testing.AllocsPerRun makes. Like
// AllocsPerRun it runs with one P so that other goroutines barely interfere.
// The count is process-wide, so a stray runtime or testing allocation can
// land in it now and then; use it through requireAllocationFreeAttempt.
func allocsOfOneRun(f func()) uint64 {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.Mallocs - before.Mallocs
}

// allocationAttempts is how often requireAllocationFreeAttempt replays a
// scenario before it reports the measurements that allocated.
const allocationAttempts = 3

// requireAllocationFreeAttempt runs attempt, which replays a scenario from a
// fresh state, measures single runs of it with allocsOfOneRun and returns a
// description of every measurement that allocated, up to allocationAttempts
// times, and passes as soon as one attempt allocated nowhere. A stray
// allocation elsewhere in the process spoils one attempt now and then (about
// once in a thousand), never all of them, while a real regression allocates in
// every attempt, since each one replays the same state. Only the failures of
// the last attempt are reported.
func requireAllocationFreeAttempt(t *testing.T, attempt func() []string) {
	t.Helper()
	var failures []string
	for i := 0; i < allocationAttempts; i++ {
		if failures = attempt(); len(failures) == 0 {
			return
		}
	}
	for _, failure := range failures {
		t.Errorf("failed in all %d attempts; last attempt: %s", allocationAttempts, failure)
	}
}

// copyingParser implements only logformat.Parser, so the aggregate copies
// every field it returns into the line scratch's own map. Unlike the default
// parser, which parses only the fields a query uses, it therefore lets a line
// with very many fields inflate the scratch map. It does not allocate for
// lines whose field names it has seen before.
type copyingParser struct {
	fields map[string]string
}

func (p *copyingParser) MakeFields(maprLine, _ string) (map[string]string, error) {
	clear(p.fields)
	for rest := maprLine; rest != ""; {
		var token string
		token, rest, _ = strings.Cut(rest, "|")
		key, value, ok := strings.Cut(token, "=")
		if !ok {
			return nil, logformat.ErrIgnoreFields
		}
		p.fields[key] = value
	}
	return p.fields, nil
}

// TestProcessorRecoversFromOutlierBatch runs the reported cases end to end:
// a batch with a few very long group keys (default parser), or a few lines
// with very many fields, must not make every ordinary batch allocate once the
// outlier batch has left the retention history.
func TestProcessorRecoversFromOutlierBatch(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector drops sync.Pool items at random, so pooled " +
			"line buffers and batch scratches are reallocated")
	}
	// One P keeps every batch on the same pooled batch scratch, so the
	// ordinary batches measured reuse the scratch the outlier batch inflated
	// instead of possibly a clean one parked on another P.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var manyFields strings.Builder
	for f := 0; f < 1000; f++ {
		manyFields.WriteString("|f")
		manyFields.WriteString(strconv.Itoa(f))
		manyFields.WriteString("=v")
	}
	const query = `from STATS select count($line) group by color`
	for _, tc := range []struct {
		name     string
		parser   logformat.Parser
		outliers int
		outlier  string
		ordinary []string
	}{
		{
			name:     "keys",
			outliers: 4,
			outlier: "INFO|1002-071143|1|stats.go:56|8|15|7|0.21|471h0m21s|" +
				"MAPREDUCE:STATS|color=" + strings.Repeat("x", 60*1024),
			ordinary: retentionLines,
		},
		{
			name:     "fields",
			parser:   &copyingParser{fields: make(map[string]string)},
			outliers: 8,
			outlier:  "color=orange" + manyFields.String(),
			ordinary: []string{"host=alpha|color=orange|n=1", "host=beta|color=purple|n=2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var aggregate *Aggregate
			if tc.parser == nil {
				var err error
				aggregate, err = newAggregateFromTextForTest(query, logging.NopLogger{})
				if err != nil {
					t.Fatalf("Failed to create aggregate: %v", err)
				}
			} else {
				aggregate = newAggregateWithParser(t, query, tc.parser)
			}
			processor := NewProcessor(aggregate, "outlier")
			defer func() {
				if err := processor.Close(); err != nil {
					t.Errorf("Close() error = %v", err)
				}
			}()
			feedBatch := func(outliers int) {
				for i := 0; i < processorBatchSize; i++ {
					line := tc.ordinary[i%len(tc.ordinary)]
					if i < outliers {
						line = tc.outlier
					}
					buffer := pool.BytesBuffer.Get().(*bytes.Buffer)
					buffer.Reset()
					buffer.WriteString(line)
					if err := processor.ProcessLine(buffer, uint64(i+1), "outlier"); err != nil {
						t.Fatalf("ProcessLine() error = %v", err)
					}
				}
			}
			feedBatch(tc.outliers)
			// The outlier batch leaves the history after retentionHistory
			// ordinary batches, the last of which trims its storage.
			for b := 0; b < retentionHistory; b++ {
				feedBatch(0)
			}
			if allocs := testing.AllocsPerRun(20, func() { feedBatch(0) }); allocs != 0 {
				t.Errorf("an ordinary batch after an outlier batch made %.1f "+
					"allocations, want 0", allocs)
			}
		})
	}
}

// TestProcessorSteadyLargeLinesAllocationFree is the regression test for a
// batch budget that also charged storage the batch just processed had used:
// on a steady workload where every line has a large group key or very many
// fields, every batch shrank the largest scratches and the next batch grew
// them back, forever. Storage the last batch used is not idle and must be kept,
// so after a warm-up batch a steady batch of identical large lines must not
// allocate at all.
func TestProcessorSteadyLargeLinesAllocationFree(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector drops sync.Pool items at random, so pooled " +
			"line buffers and batch scratches are reallocated")
	}
	// One P keeps every batch on the same pooled batch scratch.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	manyFields := func(n int) string {
		var line strings.Builder
		line.WriteString("color=orange")
		for f := 1; f < n; f++ {
			line.WriteString("|f")
			line.WriteString(strconv.Itoa(f))
			line.WriteString("=v")
		}
		return line.String()
	}
	longKey := func(n int) string {
		return "INFO|1002-071143|1|stats.go:56|8|15|7|0.21|471h0m21s|" +
			"MAPREDUCE:STATS|color=" + strings.Repeat("x", n)
	}
	const query = `from STATS select count($line) group by color`
	for _, tc := range []struct {
		name    string
		copying bool
		line    string
	}{
		{name: "keys 3 KiB", line: longKey(3000)},
		{name: "keys 4 KiB", line: longKey(4000)},
		{name: "fields 150", copying: true, line: manyFields(150)},
		{name: "fields 300", copying: true, line: manyFields(300)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var aggregate *Aggregate
			if tc.copying {
				aggregate = newAggregateWithParser(t, query,
					&copyingParser{fields: make(map[string]string)})
			} else {
				var err error
				aggregate, err = newAggregateFromTextForTest(query, logging.NopLogger{})
				if err != nil {
					t.Fatalf("Failed to create aggregate: %v", err)
				}
			}
			processor := NewProcessor(aggregate, "steady")
			defer func() {
				if err := processor.Close(); err != nil {
					t.Errorf("Close() error = %v", err)
				}
			}()
			feedBatch := func() {
				for i := 0; i < processorBatchSize; i++ {
					buffer := pool.BytesBuffer.Get().(*bytes.Buffer)
					buffer.Reset()
					buffer.WriteString(tc.line)
					if err := processor.ProcessLine(buffer, uint64(i+1), "steady"); err != nil {
						t.Fatalf("ProcessLine() error = %v", err)
					}
				}
			}
			// Warm-up: the scratches grow to the line size once. The pooled
			// batch scratch may come from an earlier test, whose other line
			// sizes stay in its history and are trimmed only after
			// retentionHistory batches, so the warm-up covers that many. The
			// unmeasured warm-up run of testing.AllocsPerRun also absorbs the
			// one-off growth of sync.Pool's own queues for the recycled line
			// buffers, which is why the batch right after this one is not
			// measured on its own here; TestBatchScratchRecoversFromOutlierBatch
			// does that for the scratches without any pool involved.
			for i := 0; i <= retentionHistory; i++ {
				feedBatch()
			}
			if allocs := testing.AllocsPerRun(20, feedBatch); allocs != 0 {
				t.Errorf("a steady batch of identical large lines made %.1f "+
					"allocations, want 0", allocs)
			}
		})
	}
}

// TestProcessorVaryingKeyLengthsAllocationFree is the regression test for a
// batch budget that measured idle headroom against each scratch's own last
// line: on a statistically steady workload whose group-key lengths vary from
// line to line, scratch i settles at the longest key it has seen while its
// last line is a random draw, so about half of every scratch counted as idle,
// the budget was exceeded once keys reached a few KiB, and every batch shrank
// a third of the scratches that the next one grew back. Headroom is measured
// against the longest key of the recent batches, which such a workload keeps
// near its maximum, so after warm-up its batches reuse the scratches.
func TestProcessorVaryingKeyLengthsAllocationFree(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector drops sync.Pool items at random, so pooled " +
			"line buffers and batch scratches are reallocated")
	}
	// One P keeps every batch on the same pooled batch scratch.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	const (
		query  = `from STATS select count($line) group by color`
		groups = 64
	)
	for _, maxKey := range []int{2 * 1024, 4 * 1024, 8 * 1024, 16 * 1024} {
		t.Run(strconv.Itoa(maxKey), func(t *testing.T) {
			// A fixed seed keeps the key lengths and the line order, and
			// with them the allocation count, the same on every run.
			rng := rand.New(rand.NewPCG(uint64(maxKey), 35))
			lines := make([]string, groups)
			for g := range lines {
				// The group number keeps two groups of equal length apart.
				lines[g] = "INFO|1002-071143|1|stats.go:56|8|15|7|0.21|471h0m21s|" +
					"MAPREDUCE:STATS|color=" + strconv.Itoa(g) +
					strings.Repeat("x", rng.IntN(maxKey+1))
			}
			order := make([]int, 64*1024)
			for i := range order {
				order[i] = rng.IntN(groups)
			}
			aggregate, err := newAggregateFromTextForTest(query, logging.NopLogger{})
			if err != nil {
				t.Fatalf("Failed to create aggregate: %v", err)
			}
			processor := NewProcessor(aggregate, "varying")
			defer func() {
				if err := processor.Close(); err != nil {
					t.Errorf("Close() error = %v", err)
				}
			}()
			next := 0
			feedBatch := func() {
				for i := 0; i < processorBatchSize; i++ {
					buffer := pool.BytesBuffer.Get().(*bytes.Buffer)
					buffer.Reset()
					buffer.WriteString(lines[order[next%len(order)]])
					next++
					if err := processor.ProcessLine(buffer, uint64(i+1), "varying"); err != nil {
						t.Fatalf("ProcessLine() error = %v", err)
					}
				}
			}
			// Warm-up: every group is inserted and every scratch grows to
			// the longest keys it is going to see.
			for i := 0; i < 50; i++ {
				feedBatch()
			}
			if allocs := testing.AllocsPerRun(200, feedBatch); allocs != 0 {
				t.Errorf("a batch of group keys up to %d bytes made %.2f "+
					"allocations, want 0", maxKey, allocs)
			}
		})
	}
}

// TestProcessorSmallerBatchesAllocationFree is the regression test for a batch
// budget that measured idle headroom against the batch just processed only:
// on a steady workload of large lines, any single batch whose largest line was
// smaller -- the one-line partial batch a follow-mode Flush drains, or a full
// batch of short lines -- found the large storage idle, shrank it, and the
// next full batch of large lines grew it back (66 allocations per large batch
// for 3000 byte keys alternating with short batches). Headroom is measured
// against the largest line of the last retentionHistory batches instead, so
// such a workload reuses every scratch once warmed up.
func TestProcessorSmallerBatchesAllocationFree(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector drops sync.Pool items at random, so pooled " +
			"line buffers and batch scratches are reallocated")
	}
	// One P keeps every batch on the same pooled batch scratch.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	longKey := func(n int) string {
		return "INFO|1002-071143|1|stats.go:56|8|15|7|0.21|471h0m21s|" +
			"MAPREDUCE:STATS|color=" + strings.Repeat("x", n)
	}
	const query = `from STATS select count($line) group by color`
	for _, tc := range []struct {
		name string
		// short is the number of short lines fed after each full batch of
		// large lines; fewer than processorBatchSize are drained by Flush.
		short  int
		keyLen int
	}{
		{"flushed short line after 2700 B keys", 1, 2700},
		{"flushed short line after 3000 B keys", 1, 3000},
		{"full short batch after 3000 B keys", processorBatchSize, 3000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			large := longKey(tc.keyLen)
			// Every attempt replays the whole sequence on a fresh aggregate
			// and processor. The pooled batch scratch carries over, but the
			// warm-up covers more batches than its history holds.
			requireAllocationFreeAttempt(t, func() (failures []string) {
				aggregate, err := newAggregateFromTextForTest(query, logging.NopLogger{})
				if err != nil {
					t.Fatalf("Failed to create aggregate: %v", err)
				}
				processor := NewProcessor(aggregate, "smaller")
				defer func() {
					if err := processor.Close(); err != nil {
						t.Errorf("Close() error = %v", err)
					}
				}()
				feed := func(line string, n int) {
					for i := 0; i < n; i++ {
						buffer := pool.BytesBuffer.Get().(*bytes.Buffer)
						buffer.Reset()
						buffer.WriteString(line)
						if err := processor.ProcessLine(buffer, uint64(i+1), "smaller"); err != nil {
							t.Fatalf("ProcessLine() error = %v", err)
						}
					}
				}
				cycle := func() {
					feed(large, processorBatchSize)
					feed(retentionLines[0], tc.short)
					if err := processor.Flush(); err != nil {
						t.Fatalf("Flush() error = %v", err)
					}
				}
				// Warm-up: both groups exist and the scratches have grown.
				for i := 0; i < 2*retentionHistory; i++ {
					cycle()
				}
				// Each cycle is measured on its own, without AllocsPerRun's
				// averaging, over more cycles than the history holds.
				for i := 0; i < 2*retentionHistory; i++ {
					if allocs := allocsOfOneRun(cycle); allocs != 0 {
						failures = append(failures, fmt.Sprintf("cycle %d: a full "+
							"batch of %d byte keys followed by %d short lines made "+
							"%d allocations, want 0", i, tc.keyLen, tc.short, allocs))
					}
				}
				return failures
			})
		})
	}
}

// TestBatchScratchSmallerBatchesAllocationFree is the unit-level companion of
// TestProcessorSmallerBatchesAllocationFree for both group-key length and
// field count: full batches of large lines alternating with a one-line or a
// full batch of small lines must reuse every scratch once warmed up.
func TestBatchScratchSmallerBatchesAllocationFree(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under the race detector")
	}
	for _, tc := range []struct {
		name         string
		largeFields  int
		largeKey     int
		smallLines   int
		smallFields  int
		smallKeySize int
	}{
		{"keys 3000 B, one short line", 5, 3000, 1, 5, 20},
		{"keys 3000 B, full short batch", 5, 3000, processorBatchSize, 5, 20},
		{"fields 300, one small line", 300, 5, 1, 3, 5},
		{"fields 300, full small batch", 300, 5, processorBatchSize, 3, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Every attempt replays the whole sequence on a fresh scratch.
			requireAllocationFreeAttempt(t, func() (failures []string) {
				scratch := &batchScratch{}
				cycle := func() {
					fillBatch(scratch, tc.largeFields, tc.largeKey)
					for i := 0; i < tc.smallLines; i++ {
						fillLineScratch(scratch.line(i), tc.smallFields, tc.smallKeySize)
					}
					scratch.clear()
				}
				cycle()
				for i := 0; i < 2*retentionHistory; i++ {
					if allocs := allocsOfOneRun(cycle); allocs != 0 {
						failures = append(failures, fmt.Sprintf("cycle %d made %d "+
							"allocations, want 0", i, allocs))
					}
				}
				return failures
			})
		})
	}
}

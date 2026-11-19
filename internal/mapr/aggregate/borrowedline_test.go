package aggregate

import (
	"bytes"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/logformat"
	"github.com/mimecast/dtail/internal/protocol"
)

// TestBorrowedLineAliasesTheBuffer is the negative control for every
// retention test in this package: it asserts that a borrowed line really does
// follow later writes to its buffer. If borrowedLine ever started copying,
// this test would fail and the retention tests below would silently stop
// proving anything.
func TestBorrowedLineAliasesTheBuffer(t *testing.T) {
	buffer := pool.BytesBuffer.Get().(*bytes.Buffer)
	defer pool.RecycleBytesBuffer(buffer)

	buffer.Reset()
	buffer.WriteString("  hello  ")
	borrowed := borrowedLine(buffer)
	if borrowed != "hello" {
		t.Fatalf("borrowedLine() = %q, want %q", borrowed, "hello")
	}

	buffer.Reset()
	buffer.WriteString("  world  ")
	if borrowed != "world" {
		t.Fatalf("borrowedLine() = %q after the buffer was reused, want %q; "+
			"the retention tests rely on this aliasing to detect retained views",
			borrowed, "world")
	}
}

func TestBorrowedLineEdgeCases(t *testing.T) {
	if got := borrowedLine(nil); got != "<nil>" {
		t.Errorf("borrowedLine(nil) = %q, want %q", got, "<nil>")
	}

	buffer := pool.BytesBuffer.Get().(*bytes.Buffer)
	defer pool.RecycleBytesBuffer(buffer)
	buffer.Reset()
	if got := borrowedLine(buffer); got != "" {
		t.Errorf("borrowedLine(empty) = %q, want an empty string", got)
	}
	buffer.WriteString(" \t\n ")
	if got := borrowedLine(buffer); got != "" {
		t.Errorf("borrowedLine(whitespace) = %q, want an empty string", got)
	}
}

func TestBuildGroupKeyReusesBuffer(t *testing.T) {
	fields := map[string]string{"host": "alpha", "color": "orange"}
	combinator := protocol.AggregateGroupKeyCombinator

	key := buildGroupKey(make([]byte, 0, 8), []string{"host", "color"}, fields)
	if string(key) != "alpha"+combinator+"orange" {
		t.Fatalf("buildGroupKey() = %q", key)
	}

	// The aggregator reuses the same buffer for the next line.
	fields["host"] = "beta"
	key = buildGroupKey(key[:0], []string{"host"}, fields)
	if string(key) != "beta" {
		t.Fatalf("buildGroupKey() = %q on the reused buffer, want %q", key, "beta")
	}

	key = buildGroupKey(key[:0], nil, fields)
	if len(key) != 0 {
		t.Errorf("buildGroupKey() = %q without a group by clause, want an empty key", key)
	}
}

// TestClearLineScratchDropsBorrowedViews asserts on a scratch this test still
// owns. Asserting after recycleBatchScratch would be a use-after-return: the
// scratch is in the pool by then and another goroutine may legitimately have
// taken it out and refilled it.
func TestClearLineScratchDropsBorrowedViews(t *testing.T) {
	scratch := newLineScratch()
	scratch.fields["borrowed"] = "view"
	scratch.key = append(scratch.key[:0], "borrowed"...)
	scratch.maxFields = len(scratch.fields)

	clearLineScratch(scratch)

	if len(scratch.fields) != 0 {
		t.Errorf("clearLineScratch() left %#v in the fields map", scratch.fields)
	}
	if len(scratch.key) != 0 {
		t.Errorf("clearLineScratch() left %q in the key buffer", scratch.key)
	}
	// The map keeps the buckets it grew, so the high-water mark stays until
	// the map is replaced.
	if scratch.maxFields != 1 {
		t.Errorf("clearLineScratch() changed maxFields to %d, want 1", scratch.maxFields)
	}
}

// TestClearLineScratchReleasesOversizedStorage pins the retention limits: one
// pathological line must not park its inflated storage in the pool, while
// ordinary lines keep reusing the very same map and key array.
func TestClearLineScratchReleasesOversizedStorage(t *testing.T) {
	scratch := newLineScratch()

	scratch.fields["host"] = "alpha"
	scratch.maxFields = len(scratch.fields)
	scratch.key = append(scratch.key[:0], "alpha"...)
	fieldsID := mapIdentity(scratch.fields)
	keyID := sliceIdentity(scratch.key)

	clearLineScratch(scratch)

	if mapIdentity(scratch.fields) != fieldsID {
		t.Error("clearLineScratch() replaced the fields map after a normal line")
	}
	if sliceIdentity(scratch.key) != keyID {
		t.Error("clearLineScratch() replaced the key buffer after a normal line")
	}
	if cap(scratch.key) != scratchKeyCapacity {
		t.Errorf("key buffer capacity = %d, want the pooled %d",
			cap(scratch.key), scratchKeyCapacity)
	}

	// Now the outlier: a huge group key and a line with far more fields than
	// the retention limit.
	scratch.key = append(scratch.key[:0], make([]byte, maxRetainedScratchKeyBytes+1)...)
	for i := 0; i <= maxRetainedScratchFields; i++ {
		scratch.fields[strconv.Itoa(i)] = "value"
	}
	scratch.maxFields = len(scratch.fields)
	inflatedFieldsID := mapIdentity(scratch.fields)

	clearLineScratch(scratch)

	if cap(scratch.key) > maxRetainedScratchKeyBytes {
		t.Errorf("clearLineScratch() parked a %d byte key buffer, want at most %d",
			cap(scratch.key), maxRetainedScratchKeyBytes)
	}
	if mapIdentity(scratch.fields) == inflatedFieldsID {
		t.Error("clearLineScratch() kept the inflated fields map; clear() does not " +
			"release the buckets a huge line grew")
	}
	if len(scratch.fields) != 0 {
		t.Errorf("clearLineScratch() left %d fields behind", len(scratch.fields))
	}

	// And the replacements are reused again by the lines that follow.
	scratch.fields["host"] = "beta"
	scratch.maxFields = len(scratch.fields)
	scratch.key = append(scratch.key[:0], "beta"...)
	fieldsID = mapIdentity(scratch.fields)
	keyID = sliceIdentity(scratch.key)

	clearLineScratch(scratch)

	if mapIdentity(scratch.fields) != fieldsID || sliceIdentity(scratch.key) != keyID {
		t.Error("clearLineScratch() failed to reuse the replacement storage")
	}
}

// TestProcessLineTracksFieldHighWaterMark covers the production wiring of the
// high-water counter, which TestClearLineScratchReleasesOversizedStorage
// cannot: that test sets scratch.maxFields by hand, so deleting the update in
// parseLine leaves it green and the oversized map would be parked in the
// pool forever. This test drives parseLine with a real line and asserts the
// counter the aggregator itself recorded.
func TestProcessLineTracksFieldHighWaterMark(t *testing.T) {
	query, err := mapr.NewQuery(`from STATS select count($line) group by host`,
		logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	// A nil query leaves the parser on its AllFields plan, so every key-value
	// token of the synthetic line below really lands in the scratch map. A
	// query-configured parser would materialize only the fields the query
	// names and could never inflate the map.
	parser, err := logformat.NewParserWithHostname("default", nil, "aggregate-test")
	if err != nil {
		t.Fatalf("NewParserWithHostname() error = %v", err)
	}
	aggregate, err := New(query, parser, "aggregate-test", logging.NopLogger{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	const oversizedFields = maxRetainedScratchFields + 1
	var line strings.Builder
	line.WriteString("INFO|1002-071143|1|stats.go:56|8|15|7|0.21|471h0m21s|" +
		"MAPREDUCE:STATS|host=alpha")
	for i := 0; i < oversizedFields; i++ {
		line.WriteString("|f")
		line.WriteString(strconv.Itoa(i))
		line.WriteString("=v")
	}

	// A scratch of this test's own, never shared with a pooled batch scratch.
	scratch := newLineScratch()
	buffer := pool.BytesBuffer.Get().(*bytes.Buffer)
	defer pool.RecycleBytesBuffer(buffer)
	buffer.Reset()
	buffer.WriteString(line.String())

	if _, err := aggregate.parseLine(scratch, buffer, "highwater"); err != nil {
		t.Fatalf("parseLine() error = %v", err)
	}
	if got := len(scratch.fields); got < oversizedFields {
		t.Fatalf("test setup: the line produced %d fields, want at least %d",
			got, oversizedFields)
	}
	if scratch.maxFields != len(scratch.fields) {
		t.Errorf("parseLine() recorded maxFields = %d, want %d; the high-water "+
			"update is missing, so clearLineScratch cannot see an inflated map",
			scratch.maxFields, len(scratch.fields))
	}
	if scratch.maxFields <= maxRetainedScratchFields {
		t.Errorf("parseLine() recorded maxFields = %d, want more than the "+
			"retention limit of %d", scratch.maxFields, maxRetainedScratchFields)
	}

	// It is a high-water mark, not the last line's field count: an ordinary
	// line after the outlier must not hide the inflated map again.
	peak := scratch.maxFields
	buffer.Reset()
	buffer.WriteString(retentionLines[0])
	if _, err := aggregate.parseLine(scratch, buffer, "highwater"); err != nil {
		t.Fatalf("parseLine() error = %v", err)
	}
	if scratch.maxFields != peak {
		t.Errorf("parseLine() lowered maxFields to %d after an ordinary line, "+
			"want the peak %d", scratch.maxFields, peak)
	}

	// End to end: what parseLine recorded is what makes the recycle path drop
	// the inflated map instead of parking it in the pool.
	inflated := mapIdentity(scratch.fields)
	clearLineScratch(scratch)
	if mapIdentity(scratch.fields) == inflated {
		t.Error("clearLineScratch() parked the inflated fields map; the high-water " +
			"mark recorded by parseLine never reached it")
	}
}

// mapIdentity and sliceIdentity report which backing storage a map or slice
// header points at, so a test can tell reuse from replacement.
func mapIdentity(m map[string]string) uintptr {
	return reflect.ValueOf(m).Pointer()
}

func sliceIdentity(b []byte) uintptr {
	return reflect.ValueOf(b).Pointer()
}

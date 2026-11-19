package logformat

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unsafe"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/protocol"
)

const defaultFormatLine = "INFO|20211002-072342|1|fieldsinto_test.go:0|8|14|7|0.21|471h0m21s|" +
	"MAPREDUCE:STATS|foo=bar|bar=baz"

// TestMakeFieldsIntoMatchesMakeFields pins the contract of the optional
// FieldsIntoParser interface: for every built-in parser the allocation-free
// form must produce exactly the fields its own MakeFields produces, and it
// must replace (not extend) the contents of the caller's map. A parser that
// forgot to override the promoted defaultParser implementation would produce
// the fields of a different log format and fail here.
func TestMakeFieldsIntoMatchesMakeFields(t *testing.T) {
	csvHeader := strings.Join([]string{"name", "value"}, protocol.CSVDelimiter)
	csvData := strings.Join([]string{"alpha", "1"}, protocol.CSVDelimiter)

	cases := []struct {
		format string
		header string
		line   string
	}{
		{format: "default", line: defaultFormatLine},
		{format: "generic", line: defaultFormatLine},
		{format: "generickv", line: "first=1|malformed|second=2"},
		{format: "csv", header: csvHeader, line: csvData},
	}

	for _, testCase := range cases {
		t.Run(testCase.format, func(t *testing.T) {
			reference, err := NewParserWithHostname(testCase.format, nil, "test-host")
			if err != nil {
				t.Fatalf("NewParserWithHostname(%q) error = %v", testCase.format, err)
			}
			reused, err := NewParserWithHostname(testCase.format, nil, "test-host")
			if err != nil {
				t.Fatalf("NewParserWithHostname(%q) error = %v", testCase.format, err)
			}
			into, ok := reused.(FieldsIntoParser)
			if !ok {
				t.Fatalf("%q parser does not implement FieldsIntoParser", testCase.format)
			}

			dst := map[string]string{"stale-field": "stale-value"}
			if testCase.header != "" {
				if _, err := reference.MakeFields(testCase.header, "src"); !errors.Is(err, ErrIgnoreFields) {
					t.Fatalf("header line error = %v, want ErrIgnoreFields", err)
				}
				if err := into.MakeFieldsInto(dst, testCase.header, "src"); !errors.Is(err, ErrIgnoreFields) {
					t.Fatalf("header line error = %v, want ErrIgnoreFields", err)
				}
			}

			want, wantErr := reference.MakeFields(testCase.line, "src")
			if wantErr != nil {
				t.Fatalf("MakeFields() error = %v", wantErr)
			}
			if err := into.MakeFieldsInto(dst, testCase.line, "src"); err != nil {
				t.Fatalf("MakeFieldsInto() error = %v", err)
			}
			if !reflect.DeepEqual(want, dst) {
				t.Errorf("MakeFieldsInto() = %#v, want %#v", dst, want)
			}
			if _, found := dst["stale-field"]; found {
				t.Errorf("MakeFieldsInto() kept a field of a previous line: %#v", dst)
			}
		})
	}
}

// TestMakeFieldsIntoIgnoredLineClearsDestination makes sure an ignored line
// cannot leave the previous line's fields behind in a reused map.
func TestMakeFieldsIntoIgnoredLineClearsDestination(t *testing.T) {
	parser, err := newDefaultParser("test-host", "UTC", 0)
	if err != nil {
		t.Fatalf("newDefaultParser() error = %v", err)
	}

	dst := make(map[string]string, 8)
	if err := parser.MakeFieldsInto(dst, defaultFormatLine, "src"); err != nil {
		t.Fatalf("MakeFieldsInto() error = %v", err)
	}
	if dst["foo"] != "bar" {
		t.Fatalf("MakeFieldsInto() = %#v, want field foo=bar", dst)
	}
	if err := parser.MakeFieldsInto(dst, "not a mapreduce line", "src"); !errors.Is(err, ErrIgnoreFields) {
		t.Fatalf("MakeFieldsInto() error = %v, want ErrIgnoreFields", err)
	}
	if len(dst) != 0 {
		t.Errorf("MakeFieldsInto() left %#v behind for an ignored line", dst)
	}
}

// TestMakeFieldsIntoFallsBackToMakeFields covers parsers registered from
// outside this package, which only implement the mandatory Parser interface.
func TestMakeFieldsIntoFallsBackToMakeFields(t *testing.T) {
	dst := make(map[string]string, 4)
	fields, err := MakeFieldsInto(&testParser{}, dst, "hello", "src")
	if err != nil {
		t.Fatalf("MakeFieldsInto() error = %v", err)
	}
	if fields["line"] != "hello" {
		t.Errorf("MakeFieldsInto() = %#v, want the fallback parser's fields", fields)
	}
	if len(dst) != 0 {
		t.Errorf("MakeFieldsInto() wrote %#v into dst on the fallback path", dst)
	}
}

// TestCSVParserCopiesBorrowedHeader pins the borrowing contract of
// Parser.MakeFields for the one built-in parser that keeps part of a line:
// the CSV header row outlives the call, so it must be copied. The test hands
// the parser a string that aliases a byte slice and then overwrites that
// slice, exactly as the aggregator's buffer pool does with a recycled line.
func TestCSVParserCopiesBorrowedHeader(t *testing.T) {
	parser, err := newCSVParser("test-host", "UTC", 0)
	if err != nil {
		t.Fatalf("newCSVParser() error = %v", err)
	}

	buffer := []byte(strings.Join([]string{"name", "value"}, protocol.CSVDelimiter))
	borrowed := unsafe.String(&buffer[0], len(buffer))
	if _, headerErr := parser.MakeFields(borrowed, "src"); !errors.Is(headerErr, ErrIgnoreFields) {
		t.Fatalf("header line error = %v, want ErrIgnoreFields", headerErr)
	}

	// The caller recycles the line buffer and writes the next line into it.
	copy(buffer, strings.Repeat("X", len(buffer)))
	if !strings.Contains(borrowed, "X") {
		t.Fatal("test setup: overwriting the buffer did not change the borrowed string")
	}

	fields, err := parser.MakeFields(strings.Join([]string{"alpha", "1"}, protocol.CSVDelimiter), "src")
	if err != nil {
		t.Fatalf("MakeFields() error = %v", err)
	}
	if fields["name"] != "alpha" || fields["value"] != "1" {
		t.Errorf("MakeFields() = %#v, want the header names copied out of the borrowed line", fields)
	}
}

// TestRegisteredParsersAgreeOnFieldsInto is the backstop for the embedding
// hazard documented on FieldsIntoParser. It walks the whole parser registry
// instead of a hand-written list, so a parser added later -- including one
// only present in a proprietary build -- has to keep MakeFields and
// MakeFieldsInto in agreement. A parser that reused another one by embedding
// it would satisfy FieldsIntoParser through the promoted method and produce
// the wrong log format's fields here. Formats whose factory reports them as
// unavailable in this build (mimecast, the custom templates) are skipped.
func TestRegisteredParsersAgreeOnFieldsInto(t *testing.T) {
	canaries := []string{
		defaultFormatLine,
		"first=1|malformed|second=2",
		strings.Join([]string{"alpha", "1"}, protocol.CSVDelimiter),
	}

	// exercised records the formats that got all the way through the subtest
	// body. Without it this test would pass with zero assertions if the
	// registry were empty or every factory reported its format unavailable, so
	// an init regression dropping the built-in parsers would still report PASS.
	exercised := make(map[string]bool)

	parserFactoriesMu.RLock()
	formats := make([]string, 0, len(parserFactories))
	for format := range parserFactories {
		formats = append(formats, format)
	}
	parserFactoriesMu.RUnlock()
	sort.Strings(formats)

	for _, format := range formats {
		t.Run(format, func(t *testing.T) {
			reference, err := NewParserWithHostname(format, nil, "test-host")
			if err != nil {
				t.Skipf("%q parser is not available in this build: %v", format, err)
			}
			reused, err := NewParserWithHostname(format, nil, "test-host")
			if err != nil {
				t.Skipf("%q parser is not available in this build: %v", format, err)
			}
			into, ok := reused.(FieldsIntoParser)
			if !ok {
				// Not implementing the optional interface is fine: such a
				// parser is served by the MakeFields fallback.
				t.Skipf("%q parser does not implement FieldsIntoParser", format)
			}

			dst := make(map[string]string, 8)
			for _, line := range canaries {
				want, wantErr := reference.MakeFields(line, "src")
				gotErr := into.MakeFieldsInto(dst, line, "src")
				if !sameError(wantErr, gotErr) {
					t.Fatalf("line %q: MakeFieldsInto() error = %v, MakeFields() error = %v",
						line, gotErr, wantErr)
				}
				if wantErr != nil {
					continue
				}
				if !reflect.DeepEqual(want, dst) {
					t.Errorf("line %q: MakeFieldsInto() = %#v, MakeFields() = %#v",
						line, dst, want)
				}
			}
			// Reached only when nothing above skipped or fataled. Subtests run
			// synchronously here, so this map needs no locking.
			exercised[format] = true
		})
	}

	// The built-in parsers are always available, so they must have been
	// compared for real. This is what keeps the walk above from passing
	// vacuously.
	for _, format := range []string{"csv", "default", "generic", "generickv"} {
		if !exercised[format] {
			t.Errorf("the %q parser was never compared: it is missing from the registry "+
				"or its subtest skipped, so this test proved nothing about it", format)
		}
	}
}

// TestMakeFieldsIntoMatchesMakeFieldsWithQuery is the configured-plan half of
// TestMakeFieldsIntoMatchesMakeFields, which builds its parsers with a nil
// query and therefore only ever exercises the AllFields plan. A real query
// narrows the plan through setQuery, and a MakeFieldsInto that consulted a
// different plan than its MakeFields -- or none at all -- would only show up
// here. The unconfigured control parser makes the subtest fail if the plan
// stopped narrowing the parse, so the comparison cannot silently become one
// between two AllFields parsers again.
func TestMakeFieldsIntoMatchesMakeFieldsWithQuery(t *testing.T) {
	csvHeader := strings.Join([]string{"name", "value"}, protocol.CSVDelimiter)
	csvData := strings.Join([]string{"alpha", "1"}, protocol.CSVDelimiter)

	cases := []struct {
		format string
		query  string
		header string
		line   string
		want   []string
	}{
		{
			format: "default",
			query:  `from STATS select count(foo),$hostname group by $hostname`,
			line:   defaultFormatLine,
			want:   []string{"foo", "$hostname"},
		},
		{
			format: "generic",
			query:  `from STATS select count($line),$hostname group by $hostname`,
			line:   defaultFormatLine,
			want:   []string{"$line", "$hostname"},
		},
		{
			format: "generickv",
			query:  `from STATS select count(first),second group by second`,
			line:   "first=1|malformed|second=2",
			want:   []string{"first", "second"},
		},
		{
			format: "csv",
			query:  `from STATS select count(value),name group by name`,
			header: csvHeader,
			line:   csvData,
			want:   []string{"name", "value"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.format, func(t *testing.T) {
			query, err := mapr.NewQuery(testCase.query, logging.NopLogger{})
			if err != nil {
				t.Fatalf("NewQuery(%q) error = %v", testCase.query, err)
			}

			reference, err := NewParserWithHostname(testCase.format, query, "test-host")
			if err != nil {
				t.Fatalf("NewParserWithHostname(%q) error = %v", testCase.format, err)
			}
			reused, err := NewParserWithHostname(testCase.format, query, "test-host")
			if err != nil {
				t.Fatalf("NewParserWithHostname(%q) error = %v", testCase.format, err)
			}
			// The control: same format, no query, so it keeps the AllFields plan.
			unconfigured, err := NewParserWithHostname(testCase.format, nil, "test-host")
			if err != nil {
				t.Fatalf("NewParserWithHostname(%q) error = %v", testCase.format, err)
			}
			into, ok := reused.(FieldsIntoParser)
			if !ok {
				t.Fatalf("%q parser does not implement FieldsIntoParser", testCase.format)
			}

			dst := map[string]string{"stale-field": "stale-value"}
			if testCase.header != "" {
				// Each parser keeps its own per-source CSV header state.
				_, headerErr := reference.MakeFields(testCase.header, "src")
				if !errors.Is(headerErr, ErrIgnoreFields) {
					t.Fatalf("header line error = %v, want ErrIgnoreFields", headerErr)
				}
				intoHeaderErr := into.MakeFieldsInto(dst, testCase.header, "src")
				if !errors.Is(intoHeaderErr, ErrIgnoreFields) {
					t.Fatalf("header line error = %v, want ErrIgnoreFields", intoHeaderErr)
				}
				_, controlHeaderErr := unconfigured.MakeFields(testCase.header, "src")
				if !errors.Is(controlHeaderErr, ErrIgnoreFields) {
					t.Fatalf("header line error = %v, want ErrIgnoreFields", controlHeaderErr)
				}
			}

			want, wantErr := reference.MakeFields(testCase.line, "src")
			if wantErr != nil {
				t.Fatalf("MakeFields() error = %v", wantErr)
			}
			if intoErr := into.MakeFieldsInto(dst, testCase.line, "src"); intoErr != nil {
				t.Fatalf("MakeFieldsInto() error = %v", intoErr)
			}
			if !reflect.DeepEqual(want, dst) {
				t.Errorf("MakeFieldsInto() = %#v, want %#v", dst, want)
			}
			if _, found := dst["stale-field"]; found {
				t.Errorf("MakeFieldsInto() kept a field of a previous line: %#v", dst)
			}
			for _, field := range testCase.want {
				if _, found := dst[field]; !found {
					t.Errorf("MakeFieldsInto() = %#v, want the queried field %q", dst, field)
				}
			}

			allFields, err := unconfigured.MakeFields(testCase.line, "src")
			if err != nil {
				t.Fatalf("unconfigured MakeFields() error = %v", err)
			}
			if len(allFields) <= len(dst) {
				t.Errorf("configured plan produced %d fields and the unconfigured "+
					"AllFields parser %d; the query plan did not narrow the parse, so "+
					"this subtest is not exercising a configured plan", len(dst), len(allFields))
			}
		})
	}
}

func sameError(want, got error) bool {
	if (want == nil) != (got == nil) {
		return false
	}
	return want == nil || want.Error() == got.Error()
}

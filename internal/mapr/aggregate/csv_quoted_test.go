package aggregate

import (
	"bytes"
	"maps"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/logformat"
)

func TestCSVQuotedSourcesKeepSeparateHeaders(t *testing.T) {
	aggregate := newCSVSourceTestAggregate(t)
	first := NewProcessor(aggregate, "same.csv")
	second := NewProcessor(aggregate, "same.csv")
	feedLines(t, first, `label,value`, `"a,b",1`, `"a""b",2`)
	flushProcessor(t, first)
	feedLines(t, second, `value,label`, `3,"a,b"`, `4,"a""b"`)
	closeProcessor(t, second)
	closeProcessor(t, first)
	if got, want := groupSamples(aggregate), (map[string]int{"a,b": 2, `a"b`: 2}); !maps.Equal(got, want) {
		t.Fatalf("samples = %v, want %v", got, want)
	}
	if got := aggregate.errors.Load(); got != 0 {
		t.Fatalf("parse errors = %d", got)
	}
}

func TestCSVPreservesPhysicalLineWhitespace(t *testing.T) {
	// The parser capability, not the query name, controls whitespace: a parser
	// selected from configuration may accompany a query without logformat csv.
	query, err := mapr.NewQuery(csvSourceTestQuery, logging.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	query.LogFormat = ""
	parser, err := logformat.NewParser("csv", query)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := New(query, parser, "test", logging.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	processor := NewProcessor(aggregate, "whitespace.csv")
	feedLines(t, processor, "label\r\n", "Jane \n", "Jane\n", " leading\r\n", "\t\u00a0 \n", "last\t")
	closeProcessor(t, processor)
	want := map[string]int{"Jane ": 1, "Jane": 1, " leading": 1, "\t\u00a0 ": 1, "last\t": 1}
	if got := groupSamples(aggregate); !maps.Equal(got, want) {
		t.Fatalf("samples = %#v, want %#v", got, want)
	}
	if got := aggregate.errors.Load(); got != 0 {
		t.Fatalf("parse errors = %d", got)
	}
}

func TestCSVPhysicalLineRejectsEmbeddedNewlines(t *testing.T) {
	aggregate := newCSVSourceTestAggregate(t)
	scratch := &lineScratch{fields: make(map[string]string)}
	if ok, err := aggregate.parseLine(scratch, bytes.NewBufferString("label\n"), "source"); ok || err != nil {
		t.Fatalf("header: parsed=%v err=%v", ok, err)
	}
	for _, value := range []string{"bare\r", "\rleading\n", "embedded\rCR\n", "double\r\r\n", "embedded\nLF\n", "double\n\n"} {
		if ok, err := aggregate.parseLine(scratch, bytes.NewBufferString(value), "source"); ok || err == nil || !strings.Contains(err.Error(), "multiline CSV") {
			t.Fatalf("value %q: parsed=%v err=%v", value, ok, err)
		}
	}
}

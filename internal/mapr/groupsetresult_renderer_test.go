package mapr

import (
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

func TestGroupSetResultUsesProvidedRenderer(t *testing.T) {
	query, queryErr := NewQuery("select host,count(value) from stats group by host order by count(value)", logging.NopLogger{})
	if queryErr != nil {
		t.Fatalf("Unable to parse query: %v", queryErr)
	}

	groupSet := NewGroupSet(logging.NopLogger{})
	set := groupSet.GetSet("host-a")
	if err := set.Aggregate("host", Last, "host-a", false); err != nil {
		t.Fatalf("Unable to aggregate host field: %v", err)
	}
	if err := set.Aggregate("count(value)", Count, "", false); err != nil {
		t.Fatalf("Unable to aggregate count field: %v", err)
	}

	renderer := &recordingRenderer{}
	result, numRows, err := groupSet.Result(query, 10, renderer)
	if err != nil {
		t.Fatalf("Unable to render result: %v", err)
	}
	if numRows != 1 {
		t.Fatalf("Expected one row, got %d", numRows)
	}
	if len(renderer.headerCalls) != 2 {
		t.Fatalf("Expected two header calls, got %d", len(renderer.headerCalls))
	}
	if renderer.headerCalls[0].isSortKey || !renderer.headerCalls[0].isGroupKey {
		t.Fatalf("Unexpected flags for group key header: %+v", renderer.headerCalls[0])
	}
	if !renderer.headerCalls[1].isSortKey || renderer.headerCalls[1].isGroupKey {
		t.Fatalf("Unexpected flags for sort key header: %+v", renderer.headerCalls[1])
	}
	if len(renderer.headerDelimiters) == 0 {
		t.Fatal("Expected header delimiters to be rendered")
	}
	if len(renderer.dataDelimiters) == 0 {
		t.Fatal("Expected data delimiters to be rendered")
	}
	if !strings.Contains(result, "host-a") || !strings.Contains(result, "1") {
		t.Fatalf("Expected rendered output to contain row data, got %q", result)
	}
}

func TestGroupSetResultFallsBackToPlainRenderer(t *testing.T) {
	query, queryErr := NewQuery("select count(value) from stats", logging.NopLogger{})
	if queryErr != nil {
		t.Fatalf("Unable to parse query: %v", queryErr)
	}

	groupSet := NewGroupSet(logging.NopLogger{})
	set := groupSet.GetSet("")
	if err := set.Aggregate("count(value)", Count, "", false); err != nil {
		t.Fatalf("Unable to aggregate count field: %v", err)
	}

	result, numRows, err := groupSet.Result(query, 10, nil)
	if err != nil {
		t.Fatalf("Unable to render result with nil renderer: %v", err)
	}
	if numRows != 1 {
		t.Fatalf("Expected one row, got %d", numRows)
	}
	if !strings.Contains(result, "count(value)") || !strings.Contains(result, "1") {
		t.Fatalf("Expected plain rendered output, got %q", result)
	}
	if strings.Contains(result, "\x1b[") {
		t.Fatalf("Expected plain output without ANSI escapes, got %q", result)
	}
}

type recordingRenderer struct {
	headerCalls      []headerCall
	headerDelimiters []string
	dataEntries      []string
	dataDelimiters   []string
}

type headerCall struct {
	text       string
	isSortKey  bool
	isGroupKey bool
}

func (r *recordingRenderer) WriteHeaderEntry(sb *strings.Builder, text string, isSortKey, isGroupKey bool) {
	r.headerCalls = append(r.headerCalls, headerCall{
		text:       text,
		isSortKey:  isSortKey,
		isGroupKey: isGroupKey,
	})
	sb.WriteString(text)
}

func (r *recordingRenderer) WriteHeaderDelimiter(sb *strings.Builder, text string) {
	r.headerDelimiters = append(r.headerDelimiters, text)
	sb.WriteString(text)
}

func (r *recordingRenderer) WriteDataEntry(sb *strings.Builder, text string) {
	r.dataEntries = append(r.dataEntries, text)
	sb.WriteString(text)
}

func (r *recordingRenderer) WriteDataDelimiter(sb *strings.Builder, text string) {
	r.dataDelimiters = append(r.dataDelimiters, text)
	sb.WriteString(text)
}

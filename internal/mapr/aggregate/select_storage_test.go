package aggregate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/client"
)

// TestAggregateRejectsDuplicateSelect checks the dserver side of the
// duplicate select check: a dserver builds its aggregate from the query an
// (older) client sends, and must refuse one that selects a storage key twice
// instead of aggregating every line into it twice.
func TestAggregateRejectsDuplicateSelect(t *testing.T) {
	for _, query := range []string{
		"from STATS select count(lat),count(lat) group by host",
		"from STATS select sum(lat),count(status),sum(lat) group by host",
		"from STATS select `count(lat)`,count(lat) group by host",
	} {
		if _, err := newAggregateFromTextForTest(query, logging.NopLogger{}); err == nil ||
			!strings.Contains(err.Error(), "'select' clause has") {
			t.Errorf("aggregate of %q: error = %v, want a duplicate select error", query, err)
		}
	}
}

// TestAggregateEveryOperationOfOneFieldOnce selects every aggregation of one
// field next to each other and feeds the lines through the real dserver path
// (processor, parser, serializer) and the client aggregation of two servers
// into the CSV outfile. Each column must hold its own value of every line
// exactly once, as each select field has its own storage key.
func TestAggregateEveryOperationOfOneFieldOnce(t *testing.T) {
	outfile := filepath.Join(t.TempDir(), "result.csv")
	queryText := "from STATS select count(lat),sum(lat),avg(lat),min(lat),max(lat)," +
		"last(lat),lat,len(lat),count(status) group by host outfile " + outfile
	aggregate, err := newAggregateFromTextForTest(queryText, logging.NopLogger{})
	if err != nil {
		t.Fatalf("Failed to create aggregate: %v", err)
	}
	processor := NewProcessor(aggregate, "storage")
	feedLines(t, processor, avgTestLine("lat=10", "status=ok"),
		avgTestLine("lat=20", "status=ok"), avgTestLine("status=ok"),
		avgTestLine("lat=30", "status=ok"))
	if err := processor.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	sent := serializeAllForTest(aggregate.serializer)
	if len(sent) != 1 {
		t.Fatalf("serialization sent %d messages, want 1: %q", len(sent), sent)
	}

	// Two servers sending the same message double every additive value.
	got := writeClientResult(t, queryText, sent[0], sent[0])
	want := "count(lat),sum(lat),avg(lat),min(lat),max(lat),last(lat),lat,len(lat),count(status)\n" +
		"6,120.000000,20.000000,10.000000,30.000000,30,30,2.000000,8\n"
	if got != want {
		t.Fatalf("outfile =\n%s\nwant\n%s", got, want)
	}
}

// writeClientResult aggregates the given server messages on the client, each
// from its own server as dmap does, and returns the written CSV outfile.
func writeClientResult(t *testing.T, queryText string, messages ...string) string {
	t.Helper()
	query, err := mapr.NewQuery(queryText, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	state := client.NewSessionState(query, logging.NopLogger{})
	for i, message := range messages {
		clientAggregate := client.NewAggregate("srv"+string(rune('1'+i)), state, logging.NopLogger{})
		if err = clientAggregate.Add([]byte(message)); err != nil {
			t.Fatalf("client Add(%q) error = %v", message, err)
		}
		if err = clientAggregate.Flush(); err != nil {
			t.Fatalf("client Flush() error = %v", err)
		}
	}
	snapshot := state.Snapshot()
	if err = snapshot.GlobalGroup.WriteResult(snapshot.Query, true); err != nil {
		t.Fatalf("WriteResult() error = %v", err)
	}
	content, err := os.ReadFile(query.Outfile.FilePath)
	if err != nil {
		t.Fatalf("read outfile: %v", err)
	}
	return string(content)
}

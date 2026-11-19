package client

import (
	"context"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
)

const avgClientTestQuery = "select avg(lat),count(status) from stats group by host"

// TestAggregateAvgValueCount verifies how the client averages the avg() sums
// of server messages: by the value count a current dserver sends with each
// sum, by the message's samples for an older dserver that sends none (so its
// average is what it always was), and by both for a mix of the two. A count
// that is not a positive number rejects the avg() field of that message (its
// sum included), while the message's other fields are still aggregated.
func TestAggregateAvgValueCount(t *testing.T) {
	countKey := mapr.AvgCountKey("avg(lat)")
	current := aggregateTestMessage("4", "avg(lat)", "30", countKey, "2", "count(status)", "4")
	older := aggregateTestMessage("4", "avg(lat)", "30", "count(status)", "4")
	badCount := func(count string) []serverMessages {
		return []serverMessages{{"srv1", []string{current,
			aggregateTestMessage("4", "avg(lat)", "1000", countKey, count, "count(status)", "4")}}}
	}
	tests := []struct {
		name      string
		servers   []serverMessages
		wantSum   float64
		wantCount int
		wantAvg   string
	}{
		{"current server", []serverMessages{{"srv1", []string{current}}}, 30, 2, "15.000000"},
		{"older server", []serverMessages{{"srv1", []string{older}}}, 30, 4, "7.500000"},
		{"current and older server",
			[]serverMessages{{"srv1", []string{current}}, {"srv2", []string{older}}}, 60, 6, "10.000000"},
		{"unparsable count", badCount("x"), 30, 2, "15.000000"},
		{"zero count", badCount("0"), 30, 2, "15.000000"},
		{"negative count", badCount("-1"), 30, 2, "15.000000"},
		{"NaN count", badCount("NaN"), 30, 2, "15.000000"},
		{"fractional count", badCount("1.5"), 30, 2, "15.000000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := aggregateServerMessages(t, avgClientTestQuery, tt.servers)
			set := state.Snapshot().GlobalGroup.GetSet("host1")
			if set.FValues["avg(lat)"] != tt.wantSum || set.ValueCounts["avg(lat)"] != tt.wantCount {
				t.Errorf("set = %v, want sum %v and value count %v", set, tt.wantSum, tt.wantCount)
			}
			// count(status) is aggregated from every message, rejected avg() or not.
			wantStatus := 0
			for _, server := range tt.servers {
				wantStatus += 4 * len(server.messages)
			}
			if got := set.FValues["count(status)"]; got != float64(wantStatus) {
				t.Errorf("count(status) = %v, want %d", got, wantStatus)
			}
			checkAvgResult(t, state, tt.wantAvg)
		})
	}
}

// TestAggregateAvgRoundTrip sends a dserver's aggregate set over the wire
// format into the client: lines without the averaged field, or with a value
// that is no number, must not lower the client's average. The set is built
// here like the dserver's serializer builds it; the real dserver path
// (processor, parser and serializer) is covered by
// TestAggregateAvgThroughSerializer in package aggregate.
func TestAggregateAvgRoundTrip(t *testing.T) {
	set := mapr.NewAggregateSet()
	for _, lat := range []string{"10", "", "abc", "20"} {
		_ = set.Aggregate("count(status)", mapr.Count, "ok", false)
		if lat != "" {
			// "abc" fails to parse and must add neither sum nor count.
			_ = set.Aggregate("avg(lat)", mapr.Avg, lat, false)
		}
		set.Samples++
	}
	ch := make(chan string, 1)
	if !set.Serialize(context.Background(), "host1", ch, logging.NopLogger{}) {
		t.Fatalf("Serialize() did not send the set")
	}
	state := aggregateServerMessages(t, avgClientTestQuery, []serverMessages{{"srv1", []string{<-ch}}})
	checkAvgResult(t, state, "15.000000")
}

// checkAvgResult checks the rendered avg(lat) of the session's result.
func checkAvgResult(t *testing.T, state *SessionState, wantAvg string) {
	t.Helper()
	snapshot := state.Snapshot()
	result, _, err := snapshot.GlobalGroup.Result(snapshot.Query, 10, nil)
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if !strings.Contains(result, " "+wantAvg+" ") {
		t.Errorf("result lacks avg(lat) %s:\n%s", wantAvg, result)
	}
}

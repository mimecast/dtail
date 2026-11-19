package aggregate

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/protocol"
)

const avgTestQuery = "from STATS select avg(lat),sum(lat),count(status) group by host"

// avgTestLine is a MAPREDUCE:STATS line of group alpha with the given fields
// appended (each "key=value").
func avgTestLine(fields ...string) string {
	line := "INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|host=alpha"
	for _, field := range fields {
		line += "|" + field
	}
	return line
}

// TestAggregateAvgThroughSerializer feeds lines through the real dserver path
// (processor, parser, serializer.aggregateLocked, serialization) into the
// real client aggregation: lines without the averaged field, or with a value
// that is no number, count as samples but must neither be sent in the avg()
// value count nor lower the client's average.
func TestAggregateAvgThroughSerializer(t *testing.T) {
	tests := []struct {
		name      string
		lines     []string
		wantCount string
		wantSum   string
		wantAvg   string
	}{
		{"lat, missing lat, lat no number",
			[]string{avgTestLine("lat=10", "status=ok"), avgTestLine("status=ok"),
				avgTestLine("lat=abc", "status=ok")}, "1", "10", "10.000000"},
		{"and a second lat",
			[]string{avgTestLine("lat=10", "status=ok"), avgTestLine("status=ok"),
				avgTestLine("lat=abc", "status=ok"), avgTestLine("lat=20", "status=ok")},
			"2", "30", "15.000000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			aggregate, err := newAggregateFromTextForTest(avgTestQuery, logging.NopLogger{})
			if err != nil {
				t.Fatalf("Failed to create aggregate: %v", err)
			}
			processor := NewProcessor(aggregate, "avg")
			feedLines(t, processor, tt.lines...)
			if err := processor.Flush(); err != nil {
				t.Fatalf("Flush() error = %v", err)
			}
			sent := serializeAllForTest(aggregate.serializer)
			if len(sent) != 1 {
				t.Fatalf("serialization sent %d messages, want 1: %q", len(sent), sent)
			}
			message := sent[0]

			// Every line has a status, so every line is a sample.
			samples := "alpha" + protocol.AggregateDelimiter + strconv.Itoa(len(tt.lines)) +
				protocol.AggregateDelimiter
			for _, want := range []string{samples,
				avgTestField("avg(lat)", tt.wantSum), avgTestField(mapr.AvgCountKey("avg(lat)"), tt.wantCount),
				avgTestField("sum(lat)", tt.wantSum)} {
				if !strings.Contains(message, want) {
					t.Errorf("message %q lacks %q", message, want)
				}
			}
			checkClientAvg(t, message, tt.wantAvg)
		})
	}
}

// avgTestField is one serialized key/value pair of a server message.
func avgTestField(key, value string) string {
	return protocol.AggregateDelimiter + key + protocol.AggregateKVDelimiter + value +
		protocol.AggregateDelimiter
}

// checkClientAvg aggregates a server message on the client as dmap does and
// checks the rendered avg(lat).
func checkClientAvg(t *testing.T, message, wantAvg string) {
	t.Helper()
	query, err := mapr.NewQuery(avgTestQuery, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	state := client.NewSessionState(query, logging.NopLogger{})
	clientAggregate := client.NewAggregate("srv1", state, logging.NopLogger{})
	if err = clientAggregate.Add([]byte(message)); err != nil {
		t.Fatalf("client Add(%q) error = %v", message, err)
	}
	if err = clientAggregate.Flush(); err != nil {
		t.Fatalf("client Flush() error = %v", err)
	}
	snapshot := state.Snapshot()
	result, _, err := snapshot.GlobalGroup.Result(snapshot.Query, 10, nil)
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if !strings.Contains(result, " "+wantAvg+" ") {
		t.Errorf("client result lacks avg(lat) %s:\n%s", wantAvg, result)
	}
}

// TestSerializerMergeRemainingAvgValueCount verifies that re-merging unsent
// groups keeps the avg() value counts: a group aggregated again meanwhile
// adds the live count to the unsent one, and a live set without any avg()
// value (only other fields) adds its samples but no count, so the average of
// the next serialization is still the sum divided by the values it holds.
func TestSerializerMergeRemainingAvgValueCount(t *testing.T) {
	query, err := mapr.NewQuery(avgTestQuery, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	s := newSerializer(query, logging.NopLogger{})
	s.groupSets["both"] = &mapr.AggregateSet{Samples: 2,
		FValues:     map[string]float64{"avg(lat)": 20, "count(status)": 2},
		SValues:     map[string]string{},
		ValueCounts: map[string]int{"avg(lat)": 1}}
	s.groupSets["nolat"] = &mapr.AggregateSet{Samples: 3,
		FValues: map[string]float64{"count(status)": 3}, SValues: map[string]string{}}

	s.mergeRemaining(map[string]*mapr.AggregateSet{
		"both": {Samples: 3, FValues: map[string]float64{"avg(lat)": 40, "count(status)": 3},
			SValues: map[string]string{}, ValueCounts: map[string]int{"avg(lat)": 2}},
		"nolat": {Samples: 1, FValues: map[string]float64{"avg(lat)": 8, "count(status)": 1},
			SValues: map[string]string{}, ValueCounts: map[string]int{"avg(lat)": 1}},
	})

	for group, want := range map[string]struct {
		samples, count int
		sum            float64
	}{"both": {5, 3, 60}, "nolat": {4, 1, 8}} {
		set := s.groupSets[group]
		if set.Samples != want.samples || set.ValueCounts["avg(lat)"] != want.count ||
			set.FValues["avg(lat)"] != want.sum {
			t.Errorf("%s: set = %v, want Samples %d, avg(lat) sum %v and value count %d",
				group, set, want.samples, want.sum, want.count)
		}
	}
	// The re-merged count travels with the next serialization.
	for _, message := range serializeAllForTest(s) {
		if strings.HasPrefix(message, "both"+protocol.AggregateDelimiter) {
			checkClientAvg(t, message, "20.000000")
		} else {
			checkClientAvg(t, message, "8.000000")
		}
	}
}

// serializeAllForTest runs one real serialization of s and returns every
// message it sends.
func serializeAllForTest(s *serializer) []string {
	messages := make(chan string, len(s.groupSets)+1)
	s.prepareOutput(messages)
	s.serialize(context.Background())
	close(messages)
	var sent []string
	for message := range messages {
		sent = append(sent, message)
	}
	return sent
}

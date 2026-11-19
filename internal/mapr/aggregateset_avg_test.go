package mapr

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/protocol"
)

const avgTestQuery = "select avg(lat),count(status) from stats group by host"

// TestAggregateAvgCountsOnlyNumericValues verifies that avg() divides by the
// number of values of the averaged field: lines that lack it (only another
// selected field is aggregated) or whose value is not a number still count as
// samples, but must not lower the average (formerly avg() divided by Samples).
func TestAggregateAvgCountsOnlyNumericValues(t *testing.T) {
	tests := []struct {
		name      string
		lats      []string // "" stands for a line without the lat field
		wantAvg   float64
		wantCount int
		wantCKey  bool
	}{
		{"every line has a number", []string{"10", "20"}, 15, 2, true},
		{"line without the field", []string{"10", ""}, 10, 1, true},
		{"value that is no number", []string{"10", "abc", "20"}, 15, 2, true},
		{"no line has a number", []string{"", "abc"}, 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := NewAggregateSet()
			for _, lat := range tt.lats {
				aggregateAvgTestLine(set, lat)
			}
			if set.Samples != len(tt.lats) {
				t.Errorf("Samples = %d, want %d (every line has a status)", set.Samples, len(tt.lats))
			}
			count, ok := set.ValueCounts["avg(lat)"]
			if ok != tt.wantCKey || count != tt.wantCount {
				t.Errorf("value count = %d (present %v), want %d (present %v)",
					count, ok, tt.wantCount, tt.wantCKey)
			}
			if got := set.avg("avg(lat)"); got != tt.wantAvg {
				t.Errorf("avg = %v, want %v", got, tt.wantAvg)
			}
		})
	}
}

// aggregateAvgTestLine aggregates one line like the dserver's
// serializer.aggregateLocked does: every line has a status, lat only when it
// is not "". It is a stand-in to test AggregateSet alone; the real dserver
// path is covered by TestAggregateAvgThroughSerializer in package aggregate.
func aggregateAvgTestLine(set *AggregateSet, lat string) {
	added := set.Aggregate("count(status)", Count, "ok", false) == nil
	if lat != "" && set.Aggregate("avg(lat)", Avg, lat, false) == nil {
		added = true
	}
	if added {
		set.Samples++
	}
}

// TestAggregateAvgRejectsInvalidInput verifies the negative paths: a value
// that is no number adds neither to the sum nor to the count, and an already
// aggregated avg() value without its count is rejected rather than averaged
// with a guessed count.
func TestAggregateAvgRejectsInvalidInput(t *testing.T) {
	set := NewAggregateSet()
	if err := set.AggregateAvg("avg(lat)", "abc", 3); err == nil {
		t.Errorf("AggregateAvg(abc) error = nil, want a parse error")
	}
	if err := set.Aggregate("avg(lat)", Avg, "10", true); err == nil {
		t.Errorf("Aggregate(avg, clientAggregation) error = nil, want an error")
	}
	if len(set.FValues) != 0 || set.ValueCounts != nil {
		t.Errorf("rejected values changed the set to %v", set)
	}
}

// TestAggregateSetMergeAvgValueCount verifies that merging carries the value
// count with the sum, and that a set without any avg() value (only other
// fields) adds its samples but nothing to the average.
func TestAggregateSetMergeAvgValueCount(t *testing.T) {
	query, err := NewQuery(avgTestQuery, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	sets := []*AggregateSet{
		{Samples: 3, FValues: map[string]float64{"avg(lat)": 10, "count(status)": 3},
			ValueCounts: map[string]int{"avg(lat)": 1}},
		{Samples: 4, FValues: map[string]float64{"count(status)": 4}},
		{Samples: 2, FValues: map[string]float64{"avg(lat)": 50, "count(status)": 2},
			ValueCounts: map[string]int{"avg(lat)": 2}},
	}
	dest := NewAggregateSet()
	for _, set := range sets {
		if err := dest.Merge(query, set); err != nil {
			t.Fatalf("Merge() error = %v", err)
		}
	}
	if dest.Samples != 9 || dest.FValues["avg(lat)"] != 60 || dest.ValueCounts["avg(lat)"] != 3 {
		t.Fatalf("merged set = %v, want Samples 9, sum 60 and value count 3", dest)
	}
	if got := dest.avg("avg(lat)"); got != 20 {
		t.Errorf("avg = %v, want 20", got)
	}
}

// TestGroupSetAvgResult verifies the rendered avg(): it divides by the value
// count, falls back to Samples for a set without one (as avg() did before
// value counts existed), and shows 0 instead of NaN without any value.
func TestGroupSetAvgResult(t *testing.T) {
	query, err := NewQuery(avgTestQuery, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	tests := []struct {
		name string
		set  AggregateSet
		want string
	}{
		{"value count", AggregateSet{Samples: 4, FValues: map[string]float64{"avg(lat)": 30},
			ValueCounts: map[string]int{"avg(lat)": 2}}, "15.000000"},
		{"no value count", AggregateSet{Samples: 4, FValues: map[string]float64{"avg(lat)": 30}}, "7.500000"},
		{"no value", AggregateSet{Samples: 4, FValues: map[string]float64{"count(status)": 4}}, "0.000000"},
		{"empty set", AggregateSet{FValues: map[string]float64{}}, "0.000000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			group := NewGroupSet(logging.NopLogger{})
			set := tt.set
			set.SValues = map[string]string{}
			*group.GetSet("host1") = set
			rows, _, err := group.result(query, false)
			if err != nil {
				t.Fatalf("result() error = %v", err)
			}
			if len(rows) != 1 || rows[0].values[0] != tt.want {
				t.Errorf("rows = %v, want avg(lat) %s", rows, tt.want)
			}
		})
	}
}

// TestAggregateSetSerializeValueCount verifies the wire format: the value
// count travels under AvgCountKey, and a set without avg() values sends none.
func TestAggregateSetSerializeValueCount(t *testing.T) {
	withAvg := NewAggregateSet()
	if err := withAvg.Aggregate("avg(lat)", Avg, "10", false); err != nil {
		t.Fatalf("Aggregate() error = %v", err)
	}
	withoutAvg := NewAggregateSet()
	if err := withoutAvg.Aggregate("count(status)", Count, "ok", false); err != nil {
		t.Fatalf("Aggregate() error = %v", err)
	}
	countField := AvgCountKey("avg(lat)") + protocol.AggregateKVDelimiter + "1" + protocol.AggregateDelimiter
	for _, tt := range []struct {
		name string
		set  *AggregateSet
		want bool
	}{{"with avg", withAvg, true}, {"without avg", withoutAvg, false}} {
		ch := make(chan string, 1)
		if !tt.set.Serialize(context.Background(), "host1", ch, logging.NopLogger{}) {
			t.Fatalf("%s: Serialize() did not send", tt.name)
		}
		if message := <-ch; strings.Contains(message, countField) != tt.want {
			t.Errorf("%s: message %q carries %q = %v, want %v", tt.name, message, countField, !tt.want, tt.want)
		}
	}
}

// TestAggregateSetSerializeCountFollowsSum pins the wire order: each avg()
// count directly follows its own sum and precedes every last()/len() string,
// so a message cut short after such a string still carries the counts (the
// client would otherwise fall back to Samples without any sign of it).
func TestAggregateSetSerializeCountFollowsSum(t *testing.T) {
	set := NewAggregateSet()
	for _, add := range []struct {
		storage string
		op      AggregateOperation
		value   string
	}{
		{"avg(lat)", Avg, "10"}, {"avg(lat)", Avg, "20"}, {"avg(size)", Avg, "7"},
		{"sum(lat)", Sum, "30"}, {"last(name)", Last, "alice"}, {"len(name)", Len, "alice"},
	} {
		if err := set.Aggregate(add.storage, add.op, add.value, false); err != nil {
			t.Fatalf("Aggregate(%s) error = %v", add.storage, err)
		}
	}
	// Map iteration order is random, so serialize a few times.
	for range 20 {
		ch := make(chan string, 1)
		if !set.Serialize(context.Background(), "host1", ch, logging.NopLogger{}) {
			t.Fatalf("Serialize() did not send")
		}
		message := <-ch
		firstString := len(message)
		// len() also sends the length as a float under its key, so look for
		// the string values themselves.
		for _, storage := range []string{"last(name)", "len(name)"} {
			index := strings.Index(message, protocol.AggregateDelimiter+storage+protocol.AggregateKVDelimiter+"alice")
			if index < 0 {
				t.Fatalf("message %q lacks %s", message, storage)
			}
			firstString = min(firstString, index)
		}
		for storage, want := range map[string]string{"avg(lat)": "30", "avg(size)": "7"} {
			count := strconv.Itoa(set.ValueCounts[storage])
			pair := protocol.AggregateDelimiter + storage + protocol.AggregateKVDelimiter + want +
				protocol.AggregateDelimiter + AvgCountKey(storage) + protocol.AggregateKVDelimiter + count +
				protocol.AggregateDelimiter
			index := strings.Index(message, pair)
			if index < 0 {
				t.Fatalf("message %q lacks sum and count %q next to each other", message, pair)
			}
			if index > firstString {
				t.Errorf("message %q: count of %s after a last()/len() string", message, storage)
			}
		}
	}
}

package aggregate

import (
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
)

const mergeRemainingTestQuery = "select min(lat),max(neg),min(absent),last(name),len(name),count(status) " +
	"from stats group by host"

// TestSerializerMergeRemainingMissingFields verifies that re-merging an unsent
// group with a newer live set of it merges only the fields a set carries: a
// value the other set lacks survives, a field neither set has gets no key
// (formerly min()/max() got a 0 that the next serialization sent), min()/max()
// of two values keep the smaller/larger one, and the newer live set's last()
// and len() values (string and length together) win where both have one.
func TestSerializerMergeRemainingMissingFields(t *testing.T) {
	query, err := mapr.NewQuery(mergeRemainingTestQuery, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	s := newSerializer(query, logging.NopLogger{})
	s.groupSets["both"] = &mapr.AggregateSet{Samples: 1,
		FValues: map[string]float64{"count(status)": 1, "len(name)": 3},
		SValues: map[string]string{"last(name)": "new", "len(name)": "new"}}
	s.groupSets["minmax"] = &mapr.AggregateSet{Samples: 1,
		FValues: map[string]float64{"min(lat)": 3, "max(neg)": -7}, SValues: map[string]string{}}
	s.groupSets["liveonly"] = &mapr.AggregateSet{Samples: 2,
		FValues: map[string]float64{"count(status)": 2}, SValues: map[string]string{}}

	s.mergeRemaining(map[string]*mapr.AggregateSet{
		"both": {Samples: 2, FValues: map[string]float64{"min(lat)": 5, "max(neg)": -3,
			"count(status)": 2, "len(name)": 6},
			SValues: map[string]string{"last(name)": "old", "len(name)": "oldest"}},
		"minmax": {Samples: 1, FValues: map[string]float64{"min(lat)": 5, "max(neg)": -3},
			SValues: map[string]string{}},
		"unsentonly": {Samples: 1, FValues: map[string]float64{"min(lat)": 7},
			SValues: map[string]string{}},
	})

	checkRemainingSet(t, "both", s.groupSets["both"], 3,
		map[string]float64{"min(lat)": 5, "max(neg)": -3, "count(status)": 3, "len(name)": 3},
		map[string]string{"last(name)": "new", "len(name)": "new"})
	checkRemainingSet(t, "minmax", s.groupSets["minmax"], 2,
		map[string]float64{"min(lat)": 3, "max(neg)": -3}, map[string]string{})
	checkRemainingSet(t, "liveonly", s.groupSets["liveonly"], 2,
		map[string]float64{"count(status)": 2}, map[string]string{})
	checkRemainingSet(t, "unsentonly", s.groupSets["unsentonly"], 1,
		map[string]float64{"min(lat)": 7}, map[string]string{})
}

// TestSerializerMergeRemainingKeepsLiveSetOnMergeError verifies the error path
// of mergeRemaining: a merge that fails (only an unknown aggregation can) keeps
// the live set unchanged instead of a partially merged one.
func TestSerializerMergeRemainingKeepsLiveSetOnMergeError(t *testing.T) {
	query, err := mapr.NewQuery(mergeRemainingTestQuery, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	// The parser rejects unknown aggregations, so make one up after parsing.
	query.Select[len(query.Select)-1].Operation = mapr.AggregateOperation(-1)
	s := newSerializer(query, logging.NopLogger{})
	live := &mapr.AggregateSet{Samples: 1, FValues: map[string]float64{"min(lat)": 3},
		SValues: map[string]string{"last(name)": "new"}}
	s.groupSets["g"] = live

	s.mergeRemaining(map[string]*mapr.AggregateSet{
		"g": {Samples: 2, FValues: map[string]float64{"min(lat)": 1},
			SValues: map[string]string{"last(name)": "old"}},
	})

	if s.groupSets["g"] != live {
		t.Fatalf("group g = %v, want the live set kept", s.groupSets["g"])
	}
	checkRemainingSet(t, "g", live, 1, map[string]float64{"min(lat)": 3},
		map[string]string{"last(name)": "new"})
}

// checkRemainingSet compares a set with the exact float and string values (and
// so also the exact storage keys) it must have.
func checkRemainingSet(t *testing.T, name string, set *mapr.AggregateSet, samples int,
	fvalues map[string]float64, svalues map[string]string) {

	t.Helper()
	if set == nil {
		t.Fatalf("%s: group missing", name)
	}
	if set.Samples != samples {
		t.Errorf("%s: Samples = %d, want %d", name, set.Samples, samples)
	}
	if len(set.FValues) != len(fvalues) || len(set.SValues) != len(svalues) {
		t.Errorf("%s: FValues = %v, SValues = %v, want %v and %v",
			name, set.FValues, set.SValues, fvalues, svalues)
	}
	for key, want := range fvalues {
		if got, ok := set.FValues[key]; !ok || got != want {
			t.Errorf("%s: %s = %v (present %v), want %v", name, key, got, ok, want)
		}
	}
	for key, want := range svalues {
		if got, ok := set.SValues[key]; !ok || got != want {
			t.Errorf("%s: %s = %q (present %v), want %q", name, key, got, ok, want)
		}
	}
}

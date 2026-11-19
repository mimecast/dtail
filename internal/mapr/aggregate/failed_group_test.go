package aggregate

import (
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
)

// A group whose every value failed to aggregate must leave no empty set
// behind: it would be sent as "key∥0∥", which the client rejects.
func TestAggregateLockedDropsGroupWithoutValues(t *testing.T) {
	query, err := mapr.NewQuery("select sum(foo) from STATS group by host", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := newSerializer(query, logging.OrNop(nil))
	s.aggregateLocked(map[string]string{"foo": "abc", "host": "a"}, []byte("a"))
	if len(s.groupSets) != 0 {
		t.Fatalf("expected no group set, got %d", len(s.groupSets))
	}

	// A failing line must not drop a group that already holds values.
	s.aggregateLocked(map[string]string{"foo": "2", "host": "a"}, []byte("a"))
	s.aggregateLocked(map[string]string{"foo": "abc", "host": "a"}, []byte("a"))
	set, ok := s.groupSets["a"]
	if !ok || set.Samples != 1 || set.FValues["sum(foo)"] != 2 {
		t.Fatalf("expected group a with one sample and sum 2, got %v", set)
	}
}

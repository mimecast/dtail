package fs

import "testing"

func TestFilteredStatsMatchSeparateUpdates(t *testing.T) {
	var got, want stats
	// Mixed initial history is possible if a ReadFile is reused for a new
	// query: matching and transmitted buckets need not agree initially.
	for i := 0; i < 200; i++ {
		want.updatePosition()
		if i%3 == 0 {
			want.updateLineMatched()
		}
		if i%5 == 0 {
			want.updateLineTransmitted()
		}
	}
	got = want
	for i := 0; i < 1000; i++ {
		got.updatePosition()
		want.updatePosition()
		matched := i%7 < 3
		got.updateFilteredLine(matched)
		if matched {
			want.updateLineMatched()
			want.updateLineTransmitted()
		} else {
			want.updateLineNotMatched()
			want.updateLineNotTransmitted()
		}
		if got != want {
			t.Fatalf("line%d: counters/history differ: got%+v want%+v", i, got, want)
		}
	}
}

package handlers

import (
	"fmt"
	"testing"
)

// TestProbeAnswersNoCSVForADServerLog checks that a probe of a log that is no
// csv file, here dserver's own `|`-separated log with a comma in some
// messages, answers no csv format: its first line used to come back as a csv
// field name, shown by `:fields` and offered by the Tab completion.
func TestProbeAnswersNoCSVForADServerLog(t *testing.T) {
	var lines []string
	for i := range 50 {
		lines = append(lines, fmt.Sprintf("INFO|1119-%06d|134919|stats.go:104|4|16|0|0.00|"+
			"47h42m19s|MAPREDUCE:STATS|currentConnections=%d|lifetimeConnections=1", i, i))
		lines = append(lines, fmt.Sprintf("INFO|1119-%06d|134920|probe.go:10|4|16|0|0.00|"+
			"47h42m20s|Probe answered|entries=%d, errors=0", i, i))
	}
	file := writeProbeTestFile(t, t.TempDir(), "20261119.log", lines...)
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)

	answer, _ := probeFiles(t, handler, file)
	if csv, ok := answer.Formats["csv"]; ok {
		t.Fatalf("csv format answered for a dserver log: %+v", csv)
	}
	if _, ok := answer.Formats["default"].Tables["STATS"]["currentConnections"]; !ok {
		t.Fatalf("default format lacks the STATS fields: %+v", answer.Formats["default"])
	}
}

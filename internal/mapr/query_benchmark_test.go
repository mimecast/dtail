package mapr

import (
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

// BenchmarkNewQuery measures parsing a query that uses every clause, several
// aggregations, numeric and string where operators and a set function, so
// that changes to the vocabulary tables (keywords, aggregations, operators,
// functions) show up in the parse cost.
func BenchmarkNewQuery(b *testing.B) {
	const queryStr = "select count($line),sum($goroutines),avg($cgocalls)," +
		"max(lifetimeConnections),percentile($loadavg) from STATS " +
		"where $cgocalls >= 0 and $hostname hasprefix \"host\" and " +
		"$goroutines nhassuffix 9 group by $hostname,$mask " +
		"set $mask = maskdigits($hostname) rorder by count($line) " +
		"interval 5 limit 10"
	b.ReportAllocs()
	for b.Loop() {
		if _, err := NewQuery(queryStr, logging.NopLogger{}); err != nil {
			b.Fatal(err)
		}
	}
}

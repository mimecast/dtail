package logformat

import (
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
)

func BenchmarkDefaultParserMakeFields(b *testing.B) {
	input := "INFO|20211002-072342|1|default_benchmark_test.go:0|8|14|7|0.21|471h0m21s|" +
		"MAPREDUCE:STATS|foo=bar|bar=baz|qux=quux|alpha=beta|gamma=delta"

	b.Run("all_fields", func(b *testing.B) {
		parser, err := NewParser("default", nil)
		if err != nil {
			b.Fatalf("Unable to create parser: %s", err.Error())
		}

		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := parser.MakeFields(input, ""); err != nil {
				b.Fatalf("Unable to parse input: %s", err.Error())
			}
		}
	})

	b.Run("query_specific", func(b *testing.B) {
		q, err := mapr.NewQuery(`select count(foo) from STATS where bar eq "baz"`, logging.NopLogger{})
		if err != nil {
			b.Fatalf("Unable to create query: %s", err.Error())
		}
		parser, err := NewParser("default", q)
		if err != nil {
			b.Fatalf("Unable to create parser: %s", err.Error())
		}

		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := parser.MakeFields(input, ""); err != nil {
				b.Fatalf("Unable to parse input: %s", err.Error())
			}
		}
	})
}

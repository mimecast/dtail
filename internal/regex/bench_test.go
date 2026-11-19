package regex

import (
	"bytes"
	"testing"
)

func BenchmarkLiteralVsRegex(b *testing.B) {
	// Test data - typical log lines
	testLines := [][]byte{
		[]byte("2024-01-01 10:00:00 INFO Starting application"),
		[]byte("2024-01-01 10:00:01 DEBUG Loading configuration"),
		[]byte("2024-01-01 10:00:02 ERROR Failed to connect to database"),
		[]byte("2024-01-01 10:00:03 WARN Retrying connection"),
		[]byte("2024-01-01 10:00:04 INFO Connection established"),
		[]byte("2024-01-01 10:00:05 ERROR Timeout while processing request"),
		[]byte("2024-01-01 10:00:06 DEBUG Processing request ID: 12345"),
		[]byte("2024-01-01 10:00:07 INFO Request processed successfully"),
		[]byte("2024-01-01 10:00:08 ERROR Invalid input parameters"),
		[]byte("2024-01-01 10:00:09 WARN High memory usage detected"),
	}

	// Benchmark literal pattern matching (our optimization)
	b.Run("Literal_ERROR", func(b *testing.B) {
		r, _ := New("ERROR", Default)
		if !r.isLiteral {
			b.Fatal("Pattern should be detected as literal")
		}

		b.ResetTimer()
		matches := 0
		for i := 0; i < b.N; i++ {
			for _, line := range testLines {
				if r.Match(line) {
					matches++
				}
			}
		}
		_ = matches
	})

	// Force regex pattern matching for comparison
	b.Run("Regex_ERROR", func(b *testing.B) {
		// Add a harmless regex operator to force regex compilation
		r, _ := New("(?:ERROR)", Default)
		if r.isLiteral {
			b.Fatal("Pattern should not be detected as literal")
		}

		b.ResetTimer()
		matches := 0
		for i := 0; i < b.N; i++ {
			for _, line := range testLines {
				if r.Match(line) {
					matches++
				}
			}
		}
		_ = matches
	})

	// Direct bytes.Contains for reference
	b.Run("BytesContains_ERROR", func(b *testing.B) {
		pattern := []byte("ERROR")

		b.ResetTimer()
		matches := 0
		for i := 0; i < b.N; i++ {
			for _, line := range testLines {
				if bytes.Contains(line, pattern) {
					matches++
				}
			}
		}
		_ = matches
	})
}

func BenchmarkComplexPatterns(b *testing.B) {
	testLine := []byte("2024-01-01 10:00:00 ERROR Failed to connect to database server at 192.168.1.100:5432")

	patterns := []struct {
		name    string
		pattern string
	}{
		{"Simple_ERROR", "ERROR"},
		{"Simple_database", "database"},
		{"Regex_ERROR.*database", "ERROR.*database"},
		{"Regex_\\d+\\.\\d+\\.\\d+\\.\\d+", `\d+\.\d+\.\d+\.\d+`}, // IP address pattern
		{"Regex_^2024", "^2024"},
		{"Regex_5432$", "5432$"},
	}

	for _, p := range patterns {
		b.Run(p.name, func(b *testing.B) {
			r, err := New(p.pattern, Default)
			if err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			matches := 0
			for i := 0; i < b.N; i++ {
				if r.Match(testLine) {
					matches++
				}
			}
			_ = matches
		})
	}
}

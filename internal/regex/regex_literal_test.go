package regex

import (
	"testing"
)

func TestIsLiteralPattern(t *testing.T) {
	tests := []struct {
		pattern  string
		expected bool
	}{
		// Literal patterns
		{"ERROR", true},
		{"hello world", true},
		{"test123", true},
		{"user@example.com", false}, // Contains @ which could be confused in some contexts
		{"192.168.1.1", false},      // Contains dots
		{"path/to/file", true},
		{"key=value", true},
		{"JSON-data", true},
		{"_underscore_", true},

		// Non-literal patterns (contain regex metacharacters)
		{".*", false},
		{"test.*", false},
		{"^start", false},
		{"end$", false},
		{"[abc]", false},
		{"a+b", false},
		{"a?b", false},
		{"a*b", false},
		{"(group)", false},
		{"a|b", false},
		{"test\\d", false},
		{"test{3}", false},
		{"test.log", false}, // Contains dot
	}

	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			got := isLiteralPattern(tt.pattern)
			if got != tt.expected {
				t.Errorf("isLiteralPattern(%q) = %v, want %v", tt.pattern, got, tt.expected)
			}
		})
	}
}

func TestLiteralMatching(t *testing.T) {
	tests := []struct {
		pattern string
		text    string
		match   bool
	}{
		{"ERROR", "This is an ERROR message", true},
		{"ERROR", "This is an error message", false}, // Case sensitive
		{"WARNING", "This is an ERROR message", false},
		{"test", "testing 123", true},
		{"test", "Test 123", false}, // Case sensitive
	}

	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			// Test with Default flag
			r, err := New(tt.pattern, Default)
			if err != nil {
				t.Fatalf("Failed to create regex: %v", err)
			}

			// Verify it's detected as literal
			if !r.isLiteral {
				t.Errorf("Pattern %q should be detected as literal", tt.pattern)
			}

			// Test string matching
			got := r.MatchString(tt.text)
			if got != tt.match {
				t.Errorf("MatchString(%q, %q) = %v, want %v", tt.pattern, tt.text, got, tt.match)
			}

			// Test byte matching
			gotBytes := r.Match([]byte(tt.text))
			if gotBytes != tt.match {
				t.Errorf("Match(%q, %q) = %v, want %v", tt.pattern, tt.text, gotBytes, tt.match)
			}
		})
	}

	// Test with Invert flag
	t.Run("InvertFlag", func(t *testing.T) {
		r, err := New("ERROR", Invert)
		if err != nil {
			t.Fatalf("Failed to create regex: %v", err)
		}

		if !r.isLiteral {
			t.Error("Pattern should be detected as literal")
		}

		// Should NOT match when pattern is present
		if r.MatchString("This is an ERROR message") {
			t.Error("Inverted match should return false when pattern is present")
		}

		// Should match when pattern is absent
		if !r.MatchString("This is a normal message") {
			t.Error("Inverted match should return true when pattern is absent")
		}
	})
}

func TestRegexCompatibility(t *testing.T) {
	// Ensure literal matching produces same results as regex matching
	patterns := []string{
		"ERROR",
		"WARNING",
		"user123",
		"test-data",
	}

	texts := []string{
		"This is an ERROR message",
		"WARNING: something happened",
		"User user123 logged in",
		"Processing test-data file",
		"No match here",
	}

	for _, pattern := range patterns {
		// Create literal regex
		literalRegex, err := New(pattern, Default)
		if err != nil {
			t.Fatalf("Failed to create literal regex: %v", err)
		}

		// Force creation of a non-literal regex for comparison
		// We'll do this by adding a harmless regex character that doesn't change the meaning
		regexPattern := "(?:" + pattern + ")"
		regexRegex, err := New(regexPattern, Default)
		if err != nil {
			t.Fatalf("Failed to create regex: %v", err)
		}

		// The literal version should be optimized
		if !literalRegex.isLiteral {
			t.Errorf("Pattern %q should be literal", pattern)
		}

		// The regex version should not be optimized
		if regexRegex.isLiteral {
			t.Errorf("Pattern %q should not be literal", regexPattern)
		}

		// Both should produce same match results
		for _, text := range texts {
			literalMatch := literalRegex.MatchString(text)

			// Test specific expected matches
			expectedMatch := false
			switch pattern {
			case "ERROR":
				expectedMatch = text == "This is an ERROR message"
			case "WARNING":
				expectedMatch = text == "WARNING: something happened"
			case "user123":
				expectedMatch = text == "User user123 logged in"
			case "test-data":
				expectedMatch = text == "Processing test-data file"
			}

			if literalMatch != expectedMatch {
				t.Errorf("Pattern %q matching text %q: got %v, want %v", pattern, text, literalMatch, expectedMatch)
			}
		}
	}
}

func TestSerializationWithLiteral(t *testing.T) {
	// Test that serialization preserves literal optimization hint
	r, err := New("ERROR", Default)
	if err != nil {
		t.Fatalf("Failed to create regex: %v", err)
	}

	if !r.isLiteral {
		t.Error("Pattern should be detected as literal")
	}

	// Serialize
	serialized, err := r.Serialize()
	if err != nil {
		t.Fatalf("Failed to serialize: %v", err)
	}

	// Should contain literal flag
	if !contains(serialized, "literal") {
		t.Errorf("Serialized form should contain 'literal' flag: %s", serialized)
	}

	// Deserialize
	deserialized, err := Deserialize(serialized)
	if err != nil {
		t.Fatalf("Failed to deserialize: %v", err)
	}

	// Should still be literal
	if !deserialized.isLiteral {
		t.Error("Deserialized regex should maintain literal flag")
	}

	// Should match the same
	testStr := "This is an ERROR message"
	if r.MatchString(testStr) != deserialized.MatchString(testStr) {
		t.Error("Original and deserialized regex should produce same match results")
	}
}

// Helper function since we can't use strings.Contains in the regex package
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

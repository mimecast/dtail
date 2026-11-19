package integrationtests

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

// TestDGrepLiteralPatterns tests grep with literal string patterns (no regex metacharacters)
func TestDGrepLiteralPatterns(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDGrepLiteralPatterns")
	defer testLogger.WriteLogFile()

	// Create test data file with various literal patterns
	testData := `2025-07-02 10:00:00 ERROR Database connection failed
2025-07-02 10:00:01 WARNING High memory usage detected
2025-07-02 10:00:02 INFO Application started successfully
2025-07-02 10:00:03 ERROR File not found: config.yaml
2025-07-02 10:00:04 DEBUG Processing request ID 12345
2025-07-02 10:00:05 ERROR Network timeout occurred
2025-07-02 10:00:06 WARNING Disk space running low
2025-07-02 10:00:07 INFO User logged in: john.doe@example.com
2025-07-02 10:00:08 ERROR Invalid input data
2025-07-02 10:00:09 CRITICAL System shutdown initiated
`

	testFile := "literal_test_data.log.tmp"
	if err := os.WriteFile(testFile, []byte(testData), 0644); err != nil {
		t.Fatal("Failed to create test file:", err)
	}
	defer os.Remove(testFile)

	tests := []struct {
		name          string
		pattern       string
		expectedCount int
		description   string
	}{
		{
			name:          "SimpleLiteral_ERROR",
			pattern:       "ERROR",
			expectedCount: 4,
			description:   "Simple literal string 'ERROR'",
		},
		{
			name:          "SimpleLiteral_WARNING",
			pattern:       "WARNING",
			expectedCount: 2,
			description:   "Simple literal string 'WARNING'",
		},
		{
			name:          "LiteralWithSpaces",
			pattern:       "File not found",
			expectedCount: 1,
			description:   "Literal string with spaces",
		},
		{
			name:          "LiteralWithNumbers",
			pattern:       "ID 12345",
			expectedCount: 1,
			description:   "Literal string with numbers",
		},
		{
			name:          "LiteralWithEmail",
			pattern:       "john.doe@example.com",
			expectedCount: 1,
			description:   "Literal string with dots (not regex dots)",
		},
		{
			name:          "LiteralNotFound",
			pattern:       "NOTFOUND",
			expectedCount: 0,
			description:   "Literal string that doesn't exist",
		},
	}

	// Test in both serverless and server modes
	modes := []struct {
		name       string
		serverMode bool
	}{
		{"Serverless", false},
		{"ServerMode", true},
	}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					outFile := fmt.Sprintf("dgrep_literal_%s.stdout.tmp", test.name)
					defer os.Remove(outFile)

					if mode.serverMode {
						testLiteralPatternWithServer(t, testLogger, testFile, outFile, test.pattern, test.expectedCount)
					} else {
						testLiteralPatternServerless(t, testLogger, testFile, outFile, test.pattern, test.expectedCount)
					}
				})
			}
		})
	}
}

// TestDGrepRegexPatterns tests grep with actual regex patterns
func TestDGrepRegexPatterns(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDGrepRegexPatterns")
	defer testLogger.WriteLogFile()

	// Create test data file
	testData := `2025-07-02 10:00:00 ERROR Database connection failed on server db01
2025-07-02 10:00:01 WARNING High memory usage: 85%
2025-07-02 10:00:02 INFO Application v1.2.3 started
2025-07-02 10:00:03 ERROR File not found: /var/log/app.log
2025-07-02 10:00:04 DEBUG Request from 192.168.1.100
2025-07-02 10:00:05 ERROR Network timeout after 30s
2025-07-02 10:00:06 WARNING Disk usage: 90%
2025-07-02 10:00:07 INFO User 'admin' logged in from 10.0.0.1
2025-07-02 10:00:08 ERROR Parse error in line 42
2025-07-02 10:00:09 CRITICAL Emergency shutdown
2025-07-02 10:00:10 DEBUG Request from 192.168.1.101
2025-07-02 10:00:11 INFO Processing batch job #12345
2025-07-02 10:00:12 ERROR Connection reset by peer
`

	testFile := "regex_test_data.log.tmp"
	if err := os.WriteFile(testFile, []byte(testData), 0644); err != nil {
		t.Fatal("Failed to create test file:", err)
	}
	defer os.Remove(testFile)

	tests := []struct {
		name          string
		pattern       string
		expectedCount int
		description   string
	}{
		{
			name:          "CharacterClass",
			pattern:       "v1.2.[0-9]",
			expectedCount: 1,
			description:   "Character class pattern",
		},
		{
			name:          "AlternationPattern",
			pattern:       "(ERROR|CRITICAL)",
			expectedCount: 6, // 5 ERROR + 1 CRITICAL
			description:   "Alternation with parentheses",
		},
		{
			name:          "WildcardPattern",
			pattern:       "ERROR.*failed",
			expectedCount: 1,
			description:   "Wildcard pattern",
		},
		{
			name:          "IPAddressPattern",
			pattern:       "[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+",
			expectedCount: 3,
			description:   "IP address regex pattern",
		},
		{
			name:          "PercentagePattern",
			pattern:       "[0-9]+%",
			expectedCount: 2,
			description:   "Percentage pattern",
		},
		{
			name:          "AnchoredPattern",
			pattern:       "^2025-07-02 10:00:0[0-5]",
			expectedCount: 6,
			description:   "Anchored pattern with start of line",
		},
		{
			name:          "OptionalPattern",
			pattern:       "errors?",
			expectedCount: 1, // Matches 'error' in "Parse error in line 42"
			description:   "Optional character pattern",
		},
		{
			name:          "PlusQuantifier",
			pattern:       "0+1",
			expectedCount: 3, // matches '01' in db01, 10.0.0.1, and 192.168.1.101
			description:   "Plus quantifier pattern",
		},
	}

	// Test in both serverless and server modes
	modes := []struct {
		name       string
		serverMode bool
	}{
		{"Serverless", false},
		{"ServerMode", true},
	}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					outFile := fmt.Sprintf("dgrep_regex_%s.stdout.tmp", test.name)
					defer os.Remove(outFile)

					if mode.serverMode {
						testRegexPatternWithServer(t, testLogger, testFile, outFile, test.pattern, test.expectedCount)
					} else {
						testRegexPatternServerless(t, testLogger, testFile, outFile, test.pattern, test.expectedCount)
					}
				})
			}
		})
	}
}

// TestDGrepMixedPatterns tests that both literal and regex patterns work in the same session
func TestDGrepMixedPatterns(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDGrepMixedPatterns")
	defer testLogger.WriteLogFile()

	// Create test data
	testData := `ERROR: System failure
WARNING: Low memory
ERROR: Disk full
INFO: Process started
ERROR: Network down
`

	testFile := "mixed_test_data.log.tmp"
	if err := os.WriteFile(testFile, []byte(testData), 0644); err != nil {
		t.Fatal("Failed to create test file:", err)
	}
	defer os.Remove(testFile)

	// Test a sequence of literal and regex patterns
	patterns := []struct {
		pattern       string
		isRegex       bool
		expectedCount int
	}{
		{"ERROR", false, 3},           // Literal
		{"ERROR.*full", true, 1},      // Regex
		{"WARNING", false, 1},         // Literal
		{"(ERROR|WARNING)", true, 4},  // Regex
		{"Process started", false, 1}, // Literal with space
		{"^INFO:", true, 1},           // Regex with anchor
	}

	t.Run("ServerlessMode", func(t *testing.T) {
		for i, p := range patterns {
			outFile := fmt.Sprintf("mixed_%d.stdout.tmp", i)
			defer os.Remove(outFile)

			testLiteralPatternServerless(t, testLogger, testFile, outFile, p.pattern, p.expectedCount)
		}
	})
}

// Helper functions

func testLiteralPatternServerless(t *testing.T, logger *TestLogger, inFile, outFile, pattern string, expectedCount int) {
	ctx := WithTestLogger(context.Background(), logger)

	_, err := runCommand(ctx, t, outFile,
		"../dgrep",
		"--plain",
		"--cfg", "none",
		"--grep", pattern,
		inFile)

	if err != nil {
		t.Errorf("Failed to run dgrep with pattern '%s': %v", pattern, err)
		return
	}

	// Count matching lines
	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Errorf("Failed to read output file: %v", err)
		return
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	actualCount := 0
	for _, line := range lines {
		if line != "" {
			actualCount++
		}
	}

	if actualCount != expectedCount {
		t.Errorf("Pattern '%s': expected %d matches, got %d", pattern, expectedCount, actualCount)
		if actualCount > 0 && actualCount <= 10 {
			t.Errorf("Output:\n%s", string(content))
		}
	}
}

func testLiteralPatternWithServer(t *testing.T, logger *TestLogger, inFile, outFile, pattern string, expectedCount int) {
	port := getUniquePortNumber()
	bindAddress := "localhost"

	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTestLogger(ctx, logger)
	defer cancel()

	// Start dserver
	_, _, _, err := startCommand(ctx, t,
		"", "../dserver",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "error",
		"--bindAddress", bindAddress,
		"--port", fmt.Sprintf("%d", port),
	)
	if err != nil {
		t.Error(err)
		return
	}

	if err := waitForServerReady(ctx, bindAddress, port); err != nil {
		t.Error(err)
		return
	}

	err = runCommandUntilValid(ctx, t, 5, 200*time.Millisecond, outFile, "../dgrep", func() error {
		content, readErr := os.ReadFile(outFile)
		if readErr != nil {
			return fmt.Errorf("failed to read output file: %w", readErr)
		}

		lines := strings.Split(strings.TrimSpace(string(content)), "\n")
		actualCount := 0
		for _, line := range lines {
			if line != "" {
				actualCount++
			}
		}

		if actualCount != expectedCount {
			if actualCount > 0 && actualCount <= 10 {
				return fmt.Errorf("pattern %q: expected %d matches, got %d\noutput:\n%s", pattern, expectedCount, actualCount, string(content))
			}
			return fmt.Errorf("pattern %q: expected %d matches, got %d", pattern, expectedCount, actualCount)
		}
		return nil
	},
		"--plain",
		"--cfg", "none",
		"--grep", pattern,
		"--servers", fmt.Sprintf("%s:%d", bindAddress, port),
		"--trustAllHosts",
		"--noColor",
		"--files", inFile)
	cancel()
	if err != nil {
		t.Error(err)
	}
}

func testRegexPatternServerless(t *testing.T, logger *TestLogger, inFile, outFile, pattern string, expectedCount int) {
	// Same implementation as testLiteralPatternServerless
	// The regex vs literal detection happens internally
	testLiteralPatternServerless(t, logger, inFile, outFile, pattern, expectedCount)
}

func testRegexPatternWithServer(t *testing.T, logger *TestLogger, inFile, outFile, pattern string, expectedCount int) {
	// Same implementation as testLiteralPatternWithServer
	// The regex vs literal detection happens internally
	testLiteralPatternWithServer(t, logger, inFile, outFile, pattern, expectedCount)
}

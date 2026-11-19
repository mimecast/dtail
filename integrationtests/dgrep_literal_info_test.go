package integrationtests

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

// TestDGrepLiteralModeInfo verifies that the server logs info message when using literal mode
func TestDGrepLiteralModeInfo(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDGrepLiteralModeInfo")
	defer testLogger.WriteLogFile()

	// Create test data
	testData := `ERROR test line 1
WARNING test line 2
ERROR test line 3
INFO test line 4
ERROR test line 5
`
	testFile := "literal_info_test.log.tmp"
	if err := os.WriteFile(testFile, []byte(testData), 0644); err != nil {
		t.Fatal("Failed to create test file:", err)
	}
	defer os.Remove(testFile)

	// Test patterns - both literal and regex
	tests := []struct {
		name          string
		pattern       string
		expectLiteral bool
		expectedCount int
	}{
		{
			name:          "SimpleLiteral",
			pattern:       "ERROR",
			expectLiteral: true,
			expectedCount: 3,
		},
		{
			name:          "LiteralWithSpace",
			pattern:       "ERROR test",
			expectLiteral: true,
			expectedCount: 3,
		},
		{
			name:          "RegexPattern",
			pattern:       "ERROR.*line [0-9]",
			expectLiteral: false,
			expectedCount: 3,
		},
		{
			name:          "RegexAlternation",
			pattern:       "(ERROR|WARNING)",
			expectLiteral: false,
			expectedCount: 4,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			port := getUniquePortNumber()
			bindAddress := "localhost"

			ctx, cancel := context.WithCancel(context.Background())
			ctx = WithTestLogger(ctx, testLogger)
			defer cancel()

			// Start dserver with info log level to capture our message
			stdoutCh, stderrCh, _, err := startCommand(ctx, t,
				"", "../dserver",
				"--cfg", "none",
				"--logger", "stdout",
				"--logLevel", "info", // Changed from error to info
				"--bindAddress", bindAddress,
				"--port", fmt.Sprintf("%d", port),
			)
			if err != nil {
				t.Error(err)
				return
			}

			// Capture server output
			var serverOutput strings.Builder
			var outputMutex sync.Mutex
			outputDone := make(chan struct{})

			go func() {
				defer close(outputDone)
				for {
					select {
					case line := <-stdoutCh:
						outputMutex.Lock()
						serverOutput.WriteString(line)
						serverOutput.WriteString("\n")
						outputMutex.Unlock()
					case line := <-stderrCh:
						outputMutex.Lock()
						serverOutput.WriteString(line)
						serverOutput.WriteString("\n")
						outputMutex.Unlock()
					case <-ctx.Done():
						return
					}
				}
			}()

			if err := waitForServerReady(ctx, bindAddress, port); err != nil {
				t.Error(err)
				return
			}

			// Run dgrep
			outFile := fmt.Sprintf("dgrep_info_%s.stdout.tmp", test.name)
			defer os.Remove(outFile)

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

				if actualCount != test.expectedCount {
					return fmt.Errorf("pattern %q: expected %d matches, got %d", test.pattern, test.expectedCount, actualCount)
				}
				return nil
			},
				"--plain",
				"--cfg", "none",
				"--grep", test.pattern,
				"--servers", fmt.Sprintf("%s:%d", bindAddress, port),
				"--trustAllHosts",
				"--noColor",
				"--files", testFile)

			if err != nil {
				t.Errorf("Failed to run dgrep with pattern '%s': %v", test.pattern, err)
				return
			}

			// Give time for server output to be captured
			time.Sleep(500 * time.Millisecond)

			// Stop server
			cancel()

			// Wait for output capture goroutine to finish
			select {
			case <-outputDone:
			case <-time.After(2 * time.Second):
				t.Log("Warning: output capture goroutine did not finish in time")
			}

			// Check server output for literal mode message
			outputMutex.Lock()
			serverLog := serverOutput.String()
			outputMutex.Unlock()
			// The server uses structured logging, so the message appears as "pattern:|<pattern>"
			literalMsg := fmt.Sprintf("Using optimized literal string matching for pattern:|%s", test.pattern)
			containsLiteralMsg := strings.Contains(serverLog, literalMsg)

			if test.expectLiteral && !containsLiteralMsg {
				t.Errorf("Expected literal mode info message for pattern '%s' but didn't find it", test.pattern)
				t.Logf("Server output:\n%s", serverLog)
			} else if !test.expectLiteral && containsLiteralMsg {
				t.Errorf("Did not expect literal mode info message for pattern '%s' but found it", test.pattern)
				t.Logf("Server output:\n%s", serverLog)
			}
		})
	}
}

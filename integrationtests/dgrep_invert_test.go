package integrationtests

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestDGrepInvertMatchAllPatterns checks that --invert is honoured for the
// patterns "." and ".*", which without --invert select every line: inverted,
// ".*" selects no line and "." only the empty lines, as grep -v does. Both
// used to print every line.
func TestDGrepInvertMatchAllPatterns(t *testing.T) {
	skipIfNotIntegrationTest(t)
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDGrepInvertMatchAllPatterns")
	defer writeLogFileIgnoringError(testLogger)

	inFile := "dgrepinvert.log.tmp"
	if err := os.WriteFile(inFile, []byte("alpha\n\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupFiles(t, inFile)

	tests := []struct {
		name, regex, want string
	}{
		{"dot", ".", "\n"},
		{"match all", ".*", ""},
		{"literal", "alpha", "\nbeta\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name+" serverless", func(t *testing.T) {
			testDGrepInvertMatchAll(t, testLogger, inFile, tt.regex, tt.want, nil)
		})
		t.Run(tt.name+" server", func(t *testing.T) {
			server := NewTestServer(t)
			if err := server.Start("error"); err != nil {
				t.Fatal(err)
			}
			testDGrepInvertMatchAll(t, testLogger, inFile, tt.regex, tt.want, []string{
				"--servers", server.Address(), "--trustAllHosts", "--noColor",
			})
		})
	}
}

// testDGrepInvertMatchAll runs dgrep --invert with the regex, serverless when
// serverArgs is empty, and expects exactly want on stdout.
func testDGrepInvertMatchAll(t *testing.T, logger *TestLogger, inFile, regex, want string,
	serverArgs []string) {
	t.Helper()
	ctx := WithTestLogger(context.Background(), logger)
	outFile := "dgrepinvert.stdout.tmp"
	cleanupFiles(t, outFile)

	args := append([]string{"--plain", "--cfg", "none", "--invert", "--regex", regex}, serverArgs...)
	args = append(args, "--files", inFile)
	err := runCommandUntilValid(ctx, t, 5, 200*time.Millisecond, outFile, "../dgrep", func() error {
		got, err := os.ReadFile(outFile)
		if err != nil {
			return err
		}
		if string(got) != want {
			return fmt.Errorf("dgrep --invert --regex %q printed %q, want %q", regex, got, want)
		}
		return nil
	}, args...)
	if err != nil {
		t.Error(err)
	}
}

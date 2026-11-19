package integrationtests

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

// TestServerWarningsIgnoreTheServerLogLevel checks that a client gets the
// text of dserver's warnings whatever dserver's --logLevel. dserver used to
// word them with its logger, which returns nothing for a filtered level, so
// at --logLevel error or none dcat printed bare "SERVER|integrationtest|"
// lines for a missing file instead of its warnings.
func TestServerWarningsIgnoreTheServerLogLevel(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestServerWarningsIgnoreTheServerLogLevel")
	defer writeLogFileIgnoringError(testLogger)
	ctx := WithTestLogger(context.Background(), testLogger)
	missing := filepath.Join(t.TempDir(), "missing.log")
	want := []string{
		"SERVER|integrationtest|WARN|",
		"|Unable to read file(s), check server logs",
		"|Giving up to read file(s)",
	}

	for i, logLevel := range []string{"error", "none"} {
		t.Run(logLevel, func(t *testing.T) {
			server := NewTestServer(t)
			if err := server.Start(logLevel); err != nil {
				t.Fatalf("start dserver: %v", err)
			}
			outFile := fmt.Sprintf("servermessage_loglevel_%d.stdout.tmp", i)
			exitCode, err := runCommand(ctx, t, outFile, "../dcat", "--cfg", "none", "--noColor",
				"--servers", server.Address(), "--trustAllHosts", "--files", missing)
			output := readTestFile(t, outFile)
			if exitCode != 2 {
				t.Fatalf("dcat exit status = %d (%v), want 2; output:\n%s", exitCode, err, output)
			}
			for _, text := range want {
				if !strings.Contains(output, text) {
					t.Fatalf("dcat output misses %q:\n%s", text, output)
				}
			}
			for _, line := range strings.Split(output, "\n") {
				if strings.TrimSpace(line) == "SERVER|integrationtest|" {
					t.Fatalf("dcat printed an empty server message:\n%s", output)
				}
			}
		})
	}
}

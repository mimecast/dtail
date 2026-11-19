package integrationtests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pathListingStoredMessage is the debug line the client logs when a path
// listing went into its cache (interactiveProber.list).
const pathListingStoredMessage = "Path listing stored"

// tabPathsDir is the directory the path completion test lists: a short
// path relative to the working directory of dtail and dserver, so the
// edited line does not wrap on the 80 column PTY.
const tabPathsDir = "tabpaths.d"

// TestInteractiveTabCompletesRemotePaths runs dtail on a PTY and completes
// --files values with Tab from path listings, against dserver and
// serverless: the names of the directory of the file being read, which the
// background prober prefetched (a name with a comma is not offered), a file
// name with a blank (inserted escaped; the reload run with it reads that
// file: lines appended to it after the reload are printed), a
// subdirectory, and a file in that subdirectory, which nobody listed
// before, so Tab asks for it and waits for the answer.
func TestInteractiveTabCompletesRemotePaths(t *testing.T) {
	skipIfNotIntegrationTest(t)
	for _, serverless := range []bool{false, true} {
		t.Run(fmt.Sprintf("serverless=%v", serverless), func(t *testing.T) {
			testInteractiveTabCompletesRemotePaths(t, serverless)
		})
	}
}

func testInteractiveTabCompletesRemotePaths(t *testing.T, serverless bool) {
	testLogger := NewTestLogger("TestInteractiveTabCompletesRemotePaths")
	defer writeLogFileIgnoringError(testLogger)
	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()
	writeTabPathsDir(t)

	dir := tabPathsDir
	argv := []string{"../dtail", "--cfg", "none", "--logger", "stdout", "--logLevel", "debug",
		"--files", filepath.Join(dir, "beta.log"), "--grep", "MARK", "--plain", "--interactive-query"}
	var serverLogs *processOutputCollector
	if !serverless {
		var port int
		port, serverLogs = startInteractiveTestServer(ctx, t)
		argv = append(argv, "--servers", fmt.Sprintf("localhost:%d", port), "--trustAllHosts")
	}
	listed := "alpha one.log  beta.log  sub/"
	// dtail follows from the end of a file, so the completed file gets lines
	// appended all along; only a read of it prints them (neither the typed
	// lines nor a listing contain their text).
	appendTicks(ctx, t, filepath.Join(dir, "alpha one.log"), alphaTick)
	steps := []interactiveStep{
		{WaitFor: pathListingStoredMessage, Input: ":reload --files " + dir + "/\t", Raw: true},
		{WaitFor: listed, Input: "al\t", Raw: true},
		{WaitFor: dir + `/alpha\ one.log`, Input: ""},
		{WaitFor: alphaTick, Input: ":show"},
		{WaitFor: "files=" + dir + "/alpha one.log", Input: ":reload --files " + dir + "/su\t", Raw: true},
		{WaitFor: dir + "/sub/", Input: "\t", Raw: true},
		{WaitFor: dir + "/sub/xfile.log", Input: "\x15:quit"},
	}
	output, err := runInteractivePTYCommand(ctx, argv, steps)
	if err != nil {
		t.Fatalf("run interactive dtail: %v\noutput:\n%s", err, output)
	}
	for _, want := range []string{listed, dir + `/alpha\ one.log`, "reload applied successfully",
		"files=" + dir + "/alpha one.log", dir + "/sub/xfile.log"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output lacks %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "no listing of") || strings.Contains(output, "Path listing failed") {
		t.Fatalf("a listing failed or did not arrive in time:\n%s", output)
	}
	if serverLogs != nil && countSubstring(serverLogs.snapshot(), "Creating new server handler") != 1 {
		t.Fatalf("expected one SSH session on the server, logs:\n%s", serverLogs.dump())
	}
}

// alphaTick starts the lines appendTicks appends to the completed file.
const alphaTick = "MARK alpha tick"

// appendTicks appends a numbered line starting with prefix to path every
// 50 ms until the test ends. Its cleanup stops the appends before the
// cleanups registered earlier (writeTabPathsDir's) remove the file.
func appendTicks(ctx context.Context, t *testing.T, path, prefix string) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done
	})
	go func() {
		defer close(done)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if err := appendLines(path, fmt.Sprintf("%s %d", prefix, i)); err != nil {
				t.Errorf("append to %s: %v", path, err)
				return
			}
		}
	}()
}

// writeTabPathsDir writes tabPathsDir afresh and removes it after the test.
func writeTabPathsDir(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(tabPathsDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tabPathsDir) })
	for name, content := range map[string]string{
		"beta.log":       "MARK beta\n",
		"alpha one.log":  "MARK alpha\n",
		"sub/xfile.log":  "MARK x\n",
		"other,name.log": "MARK comma\n",
	} {
		path := filepath.Join(tabPathsDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

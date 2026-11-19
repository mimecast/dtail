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

func TestDCat1(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDCat1")
	defer testLogger.WriteLogFile()

	// Test in serverless mode
	t.Run("Serverless", func(t *testing.T) {
		inFiles := []string{"dcat1a.txt", "dcat1b.txt", "dcat1c.txt"}
		for _, inFile := range inFiles {
			if err := testDCat1Serverless(t, testLogger, inFile); err != nil {
				t.Error(err)
				return
			}
		}
	})

	// Test in server mode
	t.Run("ServerMode", func(t *testing.T) {
		inFiles := []string{"dcat1a.txt", "dcat1b.txt", "dcat1c.txt"}
		for _, inFile := range inFiles {
			if err := testDCat1WithServer(t, testLogger, inFile); err != nil {
				t.Error(err)
				return
			}
		}
	})
}

func testDCat1Serverless(t *testing.T, logger *TestLogger, inFile string) error {
	outFile := "dcat1.tmp"
	ctx := WithTestLogger(context.Background(), logger)

	_, err := runCommand(ctx, t, outFile,
		"../dcat", "--plain", "--cfg", "none", inFile)
	if err != nil {
		return err
	}
	if err := compareFilesWithContext(ctx, t, outFile, inFile); err != nil {
		return err
	}

	return nil
}

func testDCat1WithServer(t *testing.T, logger *TestLogger, inFile string) error {
	outFile := "dcat1.tmp"
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
		return err
	}

	if err := waitForServerReady(ctx, bindAddress, port); err != nil {
		t.Error(err)
		return err
	}

	// Run dcat against the server and wait for the full file to be available.
	err = runCommandUntilValid(ctx, t, 5, 200*time.Millisecond, outFile, "../dcat", func() error {
		return compareFilesWithContext(ctx, t, outFile, inFile)
	},
		"--plain", "--cfg", "none",
		"--servers", fmt.Sprintf("%s:%d", bindAddress, port),
		"--files", inFile,
		"--trustAllHosts",
		"--noColor")

	cancel()
	return err
}

func TestDCat1Colors(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDCat1Colors")
	defer testLogger.WriteLogFile()

	// Test in serverless mode
	t.Run("Serverless", func(t *testing.T) {
		testDCat1ColorsServerless(t, testLogger)
	})

	// Test in server mode
	t.Run("ServerMode", func(t *testing.T) {
		testDCat1ColorsWithServer(t, testLogger)
	})
}

func testDCat1ColorsServerless(t *testing.T, logger *TestLogger) {
	inFile := "dcat1a.txt"
	outFile := "dcat1colors_serverless.tmp"
	ctx := WithTestLogger(context.Background(), logger)

	// Run without --plain to get colored output
	_, err := runCommand(ctx, t, outFile,
		"../dcat", "--cfg", "none", inFile)
	if err != nil {
		t.Error(err)
		return
	}

	// Just verify it ran successfully and produced output
	info, err := os.Stat(outFile)
	if err != nil {
		t.Error("Output file not created:", err)
		return
	}
	if info.Size() == 0 {
		t.Error("Output file is empty")
		return
	}

	// Verify output contains ANSI color codes
	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Error("Failed to read output file:", err)
		return
	}
	if !strings.Contains(string(content), "\033[") {
		t.Error("Output does not contain ANSI color codes")
		return
	}

	// Log verification
	logger.LogFileComparison(outFile, "ANSI color codes", "contains check")
}

func testDCat1ColorsWithServer(t *testing.T, logger *TestLogger) {
	inFile := "dcat1a.txt"
	outFile := "dcat1colors_server.tmp"
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

	err = runCommandUntilValid(ctx, t, 5, 200*time.Millisecond, outFile, "../dcat", func() error {
		info, statErr := os.Stat(outFile)
		if statErr != nil {
			return fmt.Errorf("output file not created: %w", statErr)
		}
		if info.Size() == 0 {
			return fmt.Errorf("output file is empty")
		}

		content, readErr := os.ReadFile(outFile)
		if readErr != nil {
			return fmt.Errorf("failed to read output file: %w", readErr)
		}
		if !strings.Contains(string(content), "REMOTE") && !strings.Contains(string(content), "SERVER") {
			preview := string(content)
			if len(preview) > 500 {
				preview = preview[:500]
			}
			return fmt.Errorf("server mode output does not contain server metadata. First 500 chars:\n%s", preview)
		}
		return nil
	},
		"--cfg", "none",
		"--servers", fmt.Sprintf("%s:%d", bindAddress, port),
		"--files", inFile,
		"--trustAllHosts")
	cancel()
	if err != nil {
		t.Error(err)
		return
	}
	logger.LogFileComparison(outFile, "server metadata (REMOTE/SERVER)", "contains check")
}

func TestDCat2(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDCat2")
	defer testLogger.WriteLogFile()

	// Test in serverless mode
	t.Run("Serverless", func(t *testing.T) {
		testDCat2Serverless(t, testLogger)
	})

	// Test in server mode
	t.Run("ServerMode", func(t *testing.T) {
		testDCat2WithServer(t, testLogger)
	})
}

func testDCat2Serverless(t *testing.T, logger *TestLogger) {
	inFile := "dcat2.txt"
	expectedFile := "dcat2.txt.expected"
	outFile := "dcat2_serverless.tmp"
	ctx := WithTestLogger(context.Background(), logger)

	args := []string{"--plain", "--logLevel", "error", "--cfg", "none"}

	// Cat file 100 times in one session.
	for i := 0; i < 100; i++ {
		args = append(args, inFile)
	}

	_, err := runCommand(ctx, t, outFile, "../dcat", args...)
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesContentsWithContext(ctx, t, outFile, expectedFile); err != nil {
		t.Error(err)
		return
	}
}

func testDCat2WithServer(t *testing.T, logger *TestLogger) {
	inFile := "dcat2.txt"
	expectedFile := "dcat2.txt.expected"
	outFile := "dcat2_server.tmp"
	port := getUniquePortNumber()
	bindAddress := "localhost"

	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTestLogger(ctx, logger)
	defer cancel()

	// This is a correctness test (server output must equal the expected file),
	// not a turbo-vs-non-turbo comparison. Under DTAIL_INTEGRATION_TEST_RUN_MODE
	// the server force-disables turbo boost (see internal/config/initializer.go),
	// so the dserver started here always runs on the non-turbo path regardless of
	// any DTAIL_TURBOBOOST_* env var. Genuine turbo coverage lives in the
	// benchmark harness (benchmarks/) which runs outside integration-test mode.
	// Use higher concurrency for faster test execution.
	_, _, _, err := startCommandWithEnv(ctx, t,
		"", "../dserver",
		nil,
		"--cfg", "test_server_complete.json",
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

	// Cat file 100 times in one session.
	var files []string
	for i := 0; i < 100; i++ {
		files = append(files, inFile)
	}

	args := []string{"--plain", "--logLevel", "error", "--cfg", "none",
		"--servers", fmt.Sprintf("%s:%d", bindAddress, port),
		"--trustAllHosts", "--noColor", "--files", strings.Join(files, ",")}

	err = runCommandUntilValid(ctx, t, 5, 200*time.Millisecond, outFile, "../dcat", func() error {
		return compareFilesContentsWithContext(ctx, t, outFile, expectedFile)
	}, args...)
	cancel()
	if err != nil {
		t.Error(err)
		return
	}
}

func TestDCat3(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDCat3")
	defer testLogger.WriteLogFile()

	// Test in serverless mode
	t.Run("Serverless", func(t *testing.T) {
		testDCat3Serverless(t, testLogger)
	})

	// Test in server mode
	t.Run("ServerMode", func(t *testing.T) {
		testDCat3WithServer(t, testLogger)
	})
}

func testDCat3Serverless(t *testing.T, logger *TestLogger) {
	inFile := "dcat3.txt"
	expectedFile := "dcat3.txt.expected"
	outFile := "dcat3_serverless.tmp"
	ctx := WithTestLogger(context.Background(), logger)

	args := []string{"--plain", "--logLevel", "error", "--cfg", "none", inFile}

	// Notice, with DTAIL_INTEGRATION_TEST_RUN_MODE the DTail max line length is set
	// to 1024!
	_, err := runCommand(ctx, t, outFile, "../dcat", args...)
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesContentsWithContext(ctx, t, outFile, expectedFile); err != nil {
		t.Error(err)
		return
	}
}

func testDCat3WithServer(t *testing.T, logger *TestLogger) {
	inFile := "dcat3.txt"
	expectedFile := "dcat3.txt.expected"
	outFile := "dcat3_server.tmp"
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

	args := []string{"--plain", "--logLevel", "error", "--cfg", "none",
		"--servers", fmt.Sprintf("%s:%d", bindAddress, port),
		"--files", inFile,
		"--trustAllHosts",
		"--noColor"}

	// Notice, with DTAIL_INTEGRATION_TEST_RUN_MODE the DTail max line length is set
	// to 1024!
	err = runCommandUntilValid(ctx, t, 5, 200*time.Millisecond, outFile, "../dcat", func() error {
		return compareFilesContentsWithContext(ctx, t, outFile, expectedFile)
	}, args...)
	cancel()
	if err != nil {
		t.Error(err)
		return
	}
}

func TestDCatColors(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDCatColors")
	defer testLogger.WriteLogFile()

	// Test in serverless mode
	t.Run("Serverless", func(t *testing.T) {
		testDCatColorsServerless(t, testLogger)
	})

	// Test in server mode
	t.Run("ServerMode", func(t *testing.T) {
		testDCatColorsWithServer(t, testLogger)
	})
}

func testDCatColorsServerless(t *testing.T, logger *TestLogger) {
	inFile := "dcatcolors.txt"
	outFile := "dcatcolors_serverless.tmp"
	expectedFile := "dcatcolors.expected"
	ctx := WithTestLogger(context.Background(), logger)

	_, err := runCommand(ctx, t, outFile,
		"../dcat", "--logLevel", "error", "--cfg", "none", inFile)

	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesWithContext(ctx, t, outFile, expectedFile); err != nil {
		t.Error(err)
		return
	}
}

func testDCatColorsWithServer(t *testing.T, logger *TestLogger) {
	inFile := "dcatcolors.txt"
	outFile := "dcatcolors_server.tmp"
	expectedFile := "dcatcolors.server.expected"
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

	err = runCommandUntilValid(ctx, t, 5, 200*time.Millisecond, outFile, "../dcat", func() error {
		return compareFilesWithContext(ctx, t, outFile, expectedFile)
	},
		"--logLevel", "error", "--cfg", "none",
		"--servers", fmt.Sprintf("%s:%d", bindAddress, port),
		"--files", inFile,
		"--trustAllHosts",
		"--noColor")
	cancel()
	if err != nil {
		t.Error(err)
		return
	}
}

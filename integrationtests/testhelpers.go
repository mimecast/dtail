package integrationtests

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

// TestLogger tracks test execution details for logging
type TestLogger struct {
	mu              sync.Mutex
	commandHistory  []string
	fileComparisons []string
	testName        string
}

// NewTestLogger creates a new test logger
func NewTestLogger(testName string) *TestLogger {
	return &TestLogger{
		testName:        testName,
		commandHistory:  make([]string, 0),
		fileComparisons: make([]string, 0),
	}
}

// LogCommand logs a command execution
func (tl *TestLogger) LogCommand(cmd string, args []string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	fullCmd := fmt.Sprintf("%s %s", cmd, strings.Join(args, " "))
	tl.commandHistory = append(tl.commandHistory, fullCmd)
}

// LogFileComparison logs a file comparison
func (tl *TestLogger) LogFileComparison(fileA, fileB, method string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	comparison := fmt.Sprintf("Compared %s with %s using %s", fileA, fileB, method)
	diffCmd := fmt.Sprintf("diff -u %s %s", fileA, fileB)
	tl.fileComparisons = append(tl.fileComparisons, comparison)
	tl.fileComparisons = append(tl.fileComparisons, fmt.Sprintf("Manual verification: %s", diffCmd))
}

// WriteLogFile writes the test log to a file
func (tl *TestLogger) WriteLogFile() error {
	tl.mu.Lock()
	defer tl.mu.Unlock()

	logFile := fmt.Sprintf("%s.log", tl.testName)
	f, err := os.Create(logFile)
	if err != nil {
		return fmt.Errorf("failed to create log file: %w", err)
	}
	defer f.Close()

	fmt.Fprintf(f, "Test: %s\n", tl.testName)
	fmt.Fprintf(f, "Timestamp: %s\n\n", time.Now().Format(time.RFC3339))

	fmt.Fprintf(f, "=== EXTERNAL COMMANDS EXECUTED (in order) ===\n")
	for i, cmd := range tl.commandHistory {
		fmt.Fprintf(f, "%d. %s\n", i+1, cmd)
	}

	fmt.Fprintf(f, "\n=== FILE COMPARISONS ===\n")
	for _, comparison := range tl.fileComparisons {
		fmt.Fprintf(f, "%s\n", comparison)
	}

	return nil
}

// testLoggerKey is the context key for storing the test logger
type testLoggerKey struct{}

// WithTestLogger adds a test logger to the context
func WithTestLogger(ctx context.Context, logger *TestLogger) context.Context {
	return context.WithValue(ctx, testLoggerKey{}, logger)
}

// GetTestLogger retrieves the test logger from the context
func GetTestLogger(ctx context.Context) *TestLogger {
	if logger, ok := ctx.Value(testLoggerKey{}).(*TestLogger); ok {
		return logger
	}
	return nil
}

// cleanupTmpFiles removes all .tmp files in the current directory before test execution
func cleanupTmpFiles(t *testing.T) {
	t.Helper()
	matches, err := filepath.Glob("*.tmp")
	if err != nil {
		t.Logf("Warning: failed to glob .tmp files: %v", err)
		return
	}

	for _, file := range matches {
		if err := os.Remove(file); err != nil {
			t.Logf("Warning: failed to remove %s: %v", file, err)
		} else {
			t.Logf("Cleaned up %s", file)
		}
	}
}

// skipIfNotIntegrationTest skips the test if integration tests are not enabled
func skipIfNotIntegrationTest(t *testing.T) {
	t.Helper()
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Skip("Skipping integration test")
	}
}

// ServerConfig contains configuration for starting a test server
type ServerConfig struct {
	Port        int
	BindAddress string
	LogLevel    string
	ExtraArgs   []string
	Env         map[string]string
}

// DefaultServerConfig returns a default server configuration
func DefaultServerConfig() *ServerConfig {
	return &ServerConfig{
		Port:        getUniquePortNumber(),
		BindAddress: "localhost",
		LogLevel:    "error",
	}
}

// startTestServer starts a dserver with the given configuration
func startTestServer(ctx context.Context, t *testing.T, cfg *ServerConfig) error {
	t.Helper()
	if cfg == nil {
		cfg = DefaultServerConfig()
	}

	args := []string{
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", cfg.LogLevel,
		"--bindAddress", cfg.BindAddress,
		"--port", fmt.Sprintf("%d", cfg.Port),
	}
	args = append(args, cfg.ExtraArgs...)

	_, _, _, err := startCommandWithEnv(ctx, t, "", "../dserver", cfg.Env, args...)
	if err != nil {
		return err
	}

	return waitForServerReady(ctx, cfg.BindAddress, cfg.Port)
}

func waitForServerReady(ctx context.Context, bindAddress string, port int) error {
	address := fmt.Sprintf("%s:%d", bindAddress, port)
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	var lastOutput string

	for {
		cmd := exec.CommandContext(ctx, "../dtailhealth", "--server", address, "--no-auth-key")
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		lastErr = err
		lastOutput = strings.TrimSpace(string(out))

		if ctx.Err() != nil {
			return fmt.Errorf("wait for dserver %s: %w", address, ctx.Err())
		}
		if time.Now().After(deadline) {
			if lastOutput != "" {
				return fmt.Errorf("timed out waiting for dserver %s: %w (%s)", address, lastErr, lastOutput)
			}
			return fmt.Errorf("timed out waiting for dserver %s: %w", address, lastErr)
		}

		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait for dserver %s: %w", address, ctx.Err())
		case <-timer.C:
		}
	}
}

// createTestContext creates a context with cancel that will be cleaned up automatically
func createTestContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
	})
	return ctx, cancel
}

// createTestContextWithTimeout creates a context with a 2-minute timeout that will be cleaned up automatically
func createTestContextWithTimeout(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(func() {
		cancel()
	})
	return ctx, cancel
}

// createTestContextWithLongTimeout creates a context with a 5-minute timeout for long-running tests
func createTestContextWithLongTimeout(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(func() {
		cancel()
	})
	return ctx, cancel
}

// cleanupFiles registers files to be removed during test cleanup
func cleanupFiles(t *testing.T, files ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, file := range files {
			os.Remove(file)
		}
	})
}

// TestServer manages a test server lifecycle
type TestServer struct {
	t           *testing.T
	ctx         context.Context
	cancel      context.CancelFunc
	port        int
	bindAddress string
}

// NewTestServer creates a new test server manager
func NewTestServer(t *testing.T) *TestServer {
	t.Helper()
	ctx, cancel := createTestContext(t)
	return &TestServer{
		t:           t,
		ctx:         ctx,
		cancel:      cancel,
		port:        getUniquePortNumber(),
		bindAddress: "localhost",
	}
}

// Start starts the test server with the given log level
func (ts *TestServer) Start(logLevel string) error {
	// Add cleanup to ensure server is stopped when test ends
	ts.t.Cleanup(func() {
		ts.Stop()
		// Give the server a moment to release the port
		time.Sleep(100 * time.Millisecond)
	})
	return startTestServer(ts.ctx, ts.t, &ServerConfig{
		Port:        ts.port,
		BindAddress: ts.bindAddress,
		LogLevel:    logLevel,
	})
}

// StartWithConfig starts the test server with custom configuration
func (ts *TestServer) StartWithConfig(cfg *ServerConfig) error {
	// Add cleanup to ensure server is stopped when test ends
	ts.t.Cleanup(func() {
		ts.Stop()
		// Give the server a moment to release the port
		time.Sleep(100 * time.Millisecond)
	})
	if cfg == nil {
		cfg = &ServerConfig{}
	}
	cfg.Port = ts.port
	cfg.BindAddress = ts.bindAddress
	return startTestServer(ts.ctx, ts.t, cfg)
}

// Address returns the server address in host:port format
func (ts *TestServer) Address() string {
	return fmt.Sprintf("%s:%d", ts.bindAddress, ts.port)
}

// Stop stops the test server
func (ts *TestServer) Stop() {
	ts.cancel()
}

// CommandArgs contains common command-line arguments
type CommandArgs struct {
	Config        string
	LogLevel      string
	Logger        string
	Plain         bool
	NoColor       bool
	Servers       []string
	TrustAllHosts bool
	Files         []string
	ExtraArgs     []string
}

// NewCommandArgs creates command args with sensible defaults
func NewCommandArgs() *CommandArgs {
	return &CommandArgs{
		Config: "none",
	}
}

// ToSlice converts CommandArgs to a string slice for command execution
func (c *CommandArgs) ToSlice() []string {
	args := []string{"--cfg", c.Config}

	if c.LogLevel != "" {
		args = append(args, "--logLevel", c.LogLevel)
	}
	if c.Logger != "" {
		args = append(args, "--logger", c.Logger)
	}
	if c.Plain {
		args = append(args, "--plain")
	}
	if c.NoColor {
		args = append(args, "--noColor")
	}

	// Add ExtraArgs before server/files args for commands like dgrep where order matters
	args = append(args, c.ExtraArgs...)

	if len(c.Servers) > 0 {
		args = append(args, "--servers", strings.Join(c.Servers, ","))
	}
	if c.TrustAllHosts {
		args = append(args, "--trustAllHosts")
	}
	if len(c.Files) > 0 {
		args = append(args, "--files", strings.Join(c.Files, ","))
	}

	return args
}

// DualModeTest represents a test that runs in both serverless and server modes
type DualModeTest struct {
	Name           string
	ServerlessTest func(t *testing.T)
	ServerTest     func(t *testing.T)
}

// runDualModeTest runs a test in both serverless and server modes
func runDualModeTest(t *testing.T, test DualModeTest) {
	skipIfNotIntegrationTest(t)

	if test.ServerlessTest != nil {
		t.Run("Serverless", test.ServerlessTest)
	}

	if test.ServerTest != nil {
		t.Run("ServerMode", test.ServerTest)
	}
}

// verifyFileExists checks if a file exists and is not empty
func verifyFileExists(t *testing.T, filename string) error {
	t.Helper()

	info, err := os.Stat(filename)
	if err != nil {
		return fmt.Errorf("file %s not created: %w", filename, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("file %s is empty", filename)
	}

	return nil
}

// verifyColoredOutput verifies that output contains ANSI color codes and optionally server metadata
func verifyColoredOutput(t *testing.T, outFile string, expectServerMetadata bool) error {
	t.Helper()

	if err := verifyFileExists(t, outFile); err != nil {
		return err
	}

	content, err := os.ReadFile(outFile)
	if err != nil {
		return fmt.Errorf("failed to read output file: %w", err)
	}

	// Check for ANSI color codes
	if !strings.Contains(string(content), "\033[") {
		return fmt.Errorf("output does not contain ANSI color codes")
	}

	// Check for server metadata if expected
	if expectServerMetadata {
		if !strings.Contains(string(content), "REMOTE") && !strings.Contains(string(content), "SERVER") && !strings.Contains(string(content), "CLIENT") {
			preview := string(content)
			if len(preview) > 500 {
				preview = preview[:500]
			}
			return fmt.Errorf("server mode output does not contain server metadata. First 500 chars:\n%s", preview)
		}
	}

	return nil
}

// runCommandAndVerify runs a command and verifies the output against an expected file
func runCommandAndVerify(ctx context.Context, t *testing.T, outFile, expectedFile, cmd string, args ...string) error {
	t.Helper()

	_, err := runCommand(ctx, t, outFile, cmd, args...)
	if err != nil {
		return err
	}

	if err := compareFilesWithContext(ctx, t, outFile, expectedFile); err != nil {
		return err
	}

	return nil
}

// runCommandAndVerifyContents runs a command and verifies the output contents (ignoring order)
func runCommandAndVerifyContents(ctx context.Context, t *testing.T, outFile, expectedFile, cmd string, args ...string) error {
	t.Helper()

	_, err := runCommand(ctx, t, outFile, cmd, args...)
	if err != nil {
		return err
	}

	if err := compareFilesContentsWithContext(ctx, t, outFile, expectedFile); err != nil {
		return err
	}

	return nil
}

// TestFileSet represents a set of test files
type TestFileSet struct {
	InputFile    string
	OutputFile   string
	ExpectedFile string
	ExtraFiles   []string // Additional files to clean up (e.g., .query files)
}

// Cleanup registers all files in the set for cleanup
func (tfs *TestFileSet) Cleanup(t *testing.T) {
	t.Helper()
	files := []string{tfs.OutputFile}
	files = append(files, tfs.ExtraFiles...)
	cleanupFiles(t, files...)
}

// StandardTestPaths returns common test file paths
type StandardTestPaths struct {
	MaprTestData string
	DCat1Files   []string
	DCat2File    string
	DCat3File    string
	ColorFile    string
}

// GetStandardTestPaths returns commonly used test file paths
func GetStandardTestPaths() *StandardTestPaths {
	return &StandardTestPaths{
		MaprTestData: "mapr_testdata.log",
		DCat1Files:   []string{"dcat1a.txt", "dcat1b.txt", "dcat1c.txt", "dcat1d.txt"},
		DCat2File:    "dcat2.txt",
		DCat3File:    "dcat3.txt",
		ColorFile:    "dcatcolors.txt",
	}
}

// verifyQueryFile checks if the query file contains the expected query content
func verifyQueryFile(t *testing.T, queryFile, expectedQuery string) error {
	t.Helper()

	content, err := os.ReadFile(queryFile)
	if err != nil {
		return fmt.Errorf("failed to read query file: %w", err)
	}

	actualQuery := string(content)
	if actualQuery != expectedQuery {
		return fmt.Errorf("query mismatch:\nExpected: %s\nActual: %s", expectedQuery, actualQuery)
	}

	return nil
}

//go:build linux

package integrationtests

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
	journaltest "github.com/mimecast/dtail/internal/io/journal/testhelper"
	"github.com/mimecast/dtail/internal/protocol"
)

const (
	journalExtendedSpec          = "journal:extended.service"
	mixedSourceMinLinesPerSource = 2
	mixedSourceMinSwitches       = 2
)

func TestDJournalExtendedWithServer(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDJournalExtendedWithServer")
	defer testLogger.WriteLogFile()

	t.Run("MixedFileAndJournalFollow", func(t *testing.T) {
		testDJournalExtendedMixedSources(t, testLogger)
	})
	t.Run("PermissionRegexDenialAndSuccess", func(t *testing.T) {
		testDJournalExtendedPermissions(t, testLogger)
	})
	t.Run("MissingCapabilityDoesNotHang", func(t *testing.T) {
		testDJournalExtendedMissingCapability(t, testLogger)
	})
	t.Run("RetryFirstFailureThenSuccess", func(t *testing.T) {
		testDJournalExtendedRetry(t, testLogger)
	})
	t.Run("CleanShutdownMidFollow", func(t *testing.T) {
		testDJournalExtendedCleanShutdown(t, testLogger)
	})
	t.Run("TurboCompatibility", func(t *testing.T) {
		testDJournalExtendedTurboCompatibility(t, testLogger)
	})
	t.Run("DMapJournal", func(t *testing.T) {
		testDJournalExtendedDMap(t, testLogger)
	})
}

func testDJournalExtendedMixedSources(t *testing.T, logger *TestLogger) {
	for _, mode := range dJournalExtendedServerModes() {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			testDJournalExtendedMixedSourcesForMode(t, logger, mode)
		})
	}
}

func testDJournalExtendedMixedSourcesForMode(t *testing.T, logger *TestLogger,
	mode dJournalExtendedServerMode) {

	tmpDir := t.TempDir()
	regularFile := filepath.Join(tmpDir, "mixed.log")
	if err := os.WriteFile(regularFile, nil, 0o600); err != nil {
		t.Fatalf("create mixed regular file: %v", err)
	}

	env := newDJournalExtendedEnv(t, journaltest.Scenario{
		Units: map[string]journaltest.Invocation{
			"extended.service": {
				FollowLines: []string{
					"journal mixed 1",
					"journal mixed 2",
					"journal mixed 3",
				},
				InterLineDelay: 40 * time.Millisecond,
			},
		},
	}, []string{
		permissionForPath(regularFile),
		"readfiles:^journal:extended\\.service$",
	}, map[string]any{
		"MaxConcurrentTails": 2,
	})

	server := startDJournalExtendedServerForMode(t, logger, env, "debug", mode)
	clientCtx, clientCancel := context.WithTimeout(server.ctx, 15*time.Second)
	defer clientCancel()
	cmd, stdoutCh, stderrCh, cmdErrCh := startDJournalCommand(clientCtx, t, nil, "../dtail",
		"--plain",
		"--cfg", "none",
		"--logLevel", "error",
		"--servers", server.address,
		"--files", strings.Join([]string{regularFile, journalExtendedSpec}, ","),
		"--trustAllHosts",
		"--noColor",
	)

	server.logs.waitContains(t, "Start reading|"+regularFile, 3*time.Second)
	appendLinesAfterDelay(clientCtx, t, regularFile, []string{
		"file mixed 1",
		"file mixed 2",
		"file mixed 3",
		"file mixed 4",
		"file mixed 5",
	}, 500*time.Millisecond)

	lines := readMixedSourceLines(clientCtx, t, stdoutCh, stderrCh, server.logs)
	assertMixedSourcesInterleaved(t, lines)
	stopProcessAndWait(t, cmd, cmdErrCh, "dtail mixed sources")
}

func testDJournalExtendedPermissions(t *testing.T, logger *TestLogger) {
	scenario := journaltest.Scenario{
		Units: map[string]journaltest.Invocation{
			"extended.service": {
				Lines: []string{"journal permission allowed"},
			},
		},
	}

	denyEnv := newDJournalExtendedEnv(t, scenario, []string{
		"readfiles:^journal:other\\.service$",
	}, nil)
	denyServer := startDJournalExtendedServer(t, logger, denyEnv.configFile, denyEnv.mock.Env(), "debug", false)
	denyOut := "djournal_extended_permission_deny.stdout.tmp"
	// A permission-denied dcat exits cleanly after receiving the server WARN, so
	// its error is not meaningful here; the assertion is on the server logs.
	_, _ = runCommand(denyServer.ctx, t, denyOut, "../dcat",
		"--cfg", "none",
		"--logLevel", "error",
		"--servers", denyServer.address,
		"--files", journalExtendedSpec,
		"--trustAllHosts",
		"--noColor",
	)
	denyOutput := readTestFile(t, denyOut)
	// Poll the server logs rather than reading them synchronously: the server
	// runs with the (buffered) stdout logger, so the "No permission" diagnostic
	// can lag the client's exit by up to the logger's idle-flush interval.
	// waitContains still fails if the line never appears, so the assertion is
	// not weakened.
	denyServer.logs.waitContains(t, "No permission to read file", 5*time.Second)
	if strings.Contains(denyOutput, "journal permission allowed") {
		t.Fatalf("permission denial leaked journal output:\n%s", denyOutput)
	}
	if got := strings.TrimSpace(denyEnv.mock.Args(t)); got != "" {
		t.Fatalf("permission denial invoked journalctl unexpectedly; args:\n%s", got)
	}

	allowEnv := newDJournalExtendedEnv(t, scenario, []string{
		"readfiles:^journal:extended\\.service$",
	}, nil)
	allowServer := startDJournalExtendedServer(t, logger, allowEnv.configFile, allowEnv.mock.Env(), "error", false)
	allowOut := "djournal_extended_permission_allow.stdout.tmp"
	_, err := runCommand(allowServer.ctx, t, allowOut, "../dcat",
		"--plain",
		"--cfg", "none",
		"--logLevel", "error",
		"--servers", allowServer.address,
		"--files", journalExtendedSpec,
		"--trustAllHosts",
		"--noColor",
	)
	if err != nil {
		t.Fatalf("dcat journal with matching permission failed: %v", err)
	}
	if got, want := readTestFile(t, allowOut), "journal permission allowed\n"; got != want {
		t.Fatalf("permission allow output mismatch\ngot:\n%swant:\n%s", got, want)
	}
}

func testDJournalExtendedMissingCapability(t *testing.T, logger *TestLogger) {
	env := newDJournalExtendedEnv(t, journaltest.Scenario{}, []string{
		"readfiles:^journal:extended\\.service$",
	}, nil)
	serverEnv := env.mock.Env()
	serverEnv["PATH"] = t.TempDir()
	server := startDJournalExtendedServer(t, logger, env.configFile, serverEnv, "error", false)

	runCtx, runCancel := context.WithTimeout(server.ctx, 4*time.Second)
	defer runCancel()

	outFile := "djournal_extended_missing_capability.stdout.tmp"
	started := time.Now()
	exitCode, err := runCommand(runCtx, t, outFile, "../dcat",
		"--plain",
		"--cfg", "none",
		"--logLevel", "error",
		"--servers", server.address,
		"--files", journalExtendedSpec,
		"--trustAllHosts",
		"--noColor",
	)
	if err == nil || exitCode == 0 {
		t.Fatalf("dcat journal unexpectedly succeeded without %s", protocol.CapabilityJournalV1)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("missing capability path took too long: %s", elapsed)
	}
	got := readTestFile(t, outFile)
	if !strings.Contains(got, "journal file targets require server capability "+protocol.CapabilityJournalV1) {
		t.Fatalf("missing capability output does not explain journal support failure:\n%s", got)
	}
	if runCtx.Err() != nil {
		t.Fatalf("missing capability test hit timeout instead of returning an error: %v", runCtx.Err())
	}
}

func testDJournalExtendedRetry(t *testing.T, logger *TestLogger) {
	for _, mode := range dJournalExtendedServerModes() {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			testDJournalExtendedRetryForMode(t, logger, mode)
		})
	}
}

func testDJournalExtendedRetryForMode(t *testing.T, logger *TestLogger,
	mode dJournalExtendedServerMode) {

	const retryInterval = 300 * time.Millisecond
	env := newDJournalExtendedEnv(t, journaltest.Scenario{
		Units: map[string]journaltest.Invocation{
			"extended.service": {
				FailFirst:   1,
				FollowLines: []string{"journal retry success"},
			},
		},
	}, []string{
		"readfiles:^journal:extended\\.service$",
	}, map[string]any{
		"ReadRetryIntervalMs": int(retryInterval / time.Millisecond),
	})
	server := startDJournalExtendedServerForMode(t, logger, env, "debug", mode)

	clientCtx, clientCancel := context.WithTimeout(server.ctx, 10*time.Second)
	defer clientCancel()
	started := time.Now()
	cmd, stdoutCh, stderrCh, cmdErrCh := startDJournalCommand(clientCtx, t, nil, "../dtail",
		"--plain",
		"--cfg", "none",
		"--logLevel", "error",
		"--servers", server.address,
		"--files", journalExtendedSpec,
		"--trustAllHosts",
		"--noColor",
	)
	lines := readMatchingLines(clientCtx, t, stdoutCh, stderrCh, 1, func(line string) bool {
		return strings.Contains(line, "journal retry success")
	})
	if elapsed := time.Since(started); elapsed < retryInterval {
		t.Fatalf("journal retry returned too quickly: elapsed %s, want at least %s; lines=%v", elapsed, retryInterval, lines)
	}
	if got := len(nonEmptyLines(env.mock.Args(t))); got < 2 {
		t.Fatalf("journalctl invocations = %d, want at least 2; args:\n%s", got, env.mock.Args(t))
	}
	// Stop the follow client first so the server read goroutine returns and logs
	// its post-read "File processing complete" marker; the turbo-state assertion
	// keys on that marker, which is only emitted once the read has completed.
	stopProcessAndWait(t, cmd, cmdErrCh, "dtail retry")
	assertJournalTurboLogState(t, server.logs, mode)
}

func testDJournalExtendedCleanShutdown(t *testing.T, logger *TestLogger) {
	for _, mode := range dJournalExtendedServerModes() {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			testDJournalExtendedCleanShutdownForMode(t, logger, mode)
		})
	}
}

func testDJournalExtendedCleanShutdownForMode(t *testing.T, logger *TestLogger,
	mode dJournalExtendedServerMode) {

	env := newDJournalExtendedEnv(t, journaltest.Scenario{
		Units: map[string]journaltest.Invocation{
			"extended.service": {
				FollowLines:    []string{"journal shutdown stream"},
				InterLineDelay: 50 * time.Millisecond,
			},
		},
	}, []string{
		"readfiles:^journal:extended\\.service$",
	}, map[string]any{
		"MaxConcurrentTails": 1,
	})
	server := startDJournalExtendedServerForMode(t, logger, env, "debug", mode)

	firstCtx, firstCancel := context.WithTimeout(server.ctx, 10*time.Second)
	defer firstCancel()
	cmd, stdoutCh, stderrCh, cmdErrCh := startDJournalCommand(firstCtx, t, nil, "../dtail",
		"--plain",
		"--cfg", "none",
		"--logLevel", "error",
		"--servers", server.address,
		"--files", journalExtendedSpec,
		"--trustAllHosts",
		"--noColor",
	)
	readMatchingLines(firstCtx, t, stdoutCh, stderrCh, 1, func(line string) bool {
		return strings.Contains(line, "journal shutdown stream")
	})
	firstPID := readMockPID(t, env.mock.PIDFile)
	stopProcessAndWait(t, cmd, cmdErrCh, "dtail shutdown")
	env.mock.WaitForTerm(t, 2*time.Second)
	waitForProcessExit(t, firstPID, 2*time.Second)
	server.logs.waitContains(t, "File processing complete", 3*time.Second)
	server.logs.waitContains(t, "Command finished", 3*time.Second)
	// Assert turbo state only after the read has completed: the helper keys on
	// the post-read "File processing complete" marker (emitted strictly after the
	// turbo decision), which is available now that the follow client has stopped.
	assertJournalTurboLogState(t, server.logs, mode)

	secondCtx, secondCancel := context.WithTimeout(server.ctx, 10*time.Second)
	defer secondCancel()
	secondCmd, secondStdout, secondStderr, secondErrCh := startDJournalCommand(secondCtx, t, nil, "../dtail",
		"--plain",
		"--cfg", "none",
		"--logLevel", "error",
		"--servers", server.address,
		"--files", journalExtendedSpec,
		"--trustAllHosts",
		"--noColor",
	)
	readMatchingLines(secondCtx, t, secondStdout, secondStderr, 1, func(line string) bool {
		return strings.Contains(line, "journal shutdown stream")
	})
	stopProcessAndWait(t, secondCmd, secondErrCh, "dtail shutdown second")
}

func testDJournalExtendedTurboCompatibility(t *testing.T, logger *TestLogger) {
	run := func(t *testing.T, name string, serverEnv map[string]string) (string, *safeLineLog) {
		t.Helper()

		env := newDJournalExtendedEnv(t, journaltest.Scenario{
			Units: map[string]journaltest.Invocation{
				"extended.service": {
					Lines: []string{
						"journal turbo alpha",
						"journal turbo beta",
					},
				},
			},
		}, []string{
			"readfiles:^journal:extended\\.service$",
		}, nil)
		for key, value := range env.mock.Env() {
			serverEnv[key] = value
		}
		serverEnv["DTAIL_HOSTNAME_OVERRIDE"] = "integrationtest"
		server := startDJournalExtendedServer(t, logger, env.configFile, serverEnv, "debug", true)

		outFile := fmt.Sprintf("djournal_extended_turbo_%s.stdout.tmp", name)
		_, err := runCommand(server.ctx, t, outFile, "../dcat",
			"--plain",
			"--cfg", "none",
			"--logLevel", "error",
			"--servers", server.address,
			"--files", journalExtendedSpec,
			"--trustAllHosts",
			"--noColor",
		)
		if err != nil {
			t.Fatalf("dcat turbo %s failed: %v", name, err)
		}
		return readTestFile(t, outFile), server.logs
	}

	// The direct read path is now the one and only runtime path, so this is a
	// single run. It preserves the real regression coverage: the exact journal
	// payload and proof the optimized read path ran.
	enabledOutput, enabledLogs := run(t, "enabled", map[string]string{})
	if want := "journal turbo alpha\njournal turbo beta\n"; enabledOutput != want {
		t.Fatalf("turbo compatibility output mismatch\ngot:\n%swant:\n%s\nlogs:\n%s",
			enabledOutput, want, enabledLogs.String())
	}
	enabledLogs.waitContains(t, "Using turbo mode for reading", 3*time.Second)
}

func testDJournalExtendedDMap(t *testing.T, logger *TestLogger) {
	var expectedCSV string
	for _, mode := range dJournalExtendedServerModes() {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			csv := testDJournalExtendedDMapForMode(t, logger, mode)
			if expectedCSV == "" {
				expectedCSV = csv
				return
			}
			if csv != expectedCSV {
				t.Fatalf("dmap journal output mismatch with prior turbo mode\ngot:\n%swant:\n%s", csv, expectedCSV)
			}
		})
	}
}

func testDJournalExtendedDMapForMode(t *testing.T, logger *TestLogger,
	mode dJournalExtendedServerMode) string {

	env := newDJournalExtendedEnv(t, journaltest.Scenario{
		Units: map[string]journaltest.Invocation{
			"extended.service": {
				Lines: []string{
					"INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1",
					"INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1",
					"INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1",
				},
			},
		},
	}, []string{
		"readfiles:^journal:extended\\.service$",
	}, nil)
	server := startDJournalExtendedServerForMode(t, logger, env, "debug", mode)

	csvFile := filepath.Join(t.TempDir(), fmt.Sprintf("djournal_extended_dmap_%s.csv", mode.name))
	queryFile := fmt.Sprintf("%s.query", csvFile)
	query := fmt.Sprintf("from STATS select count($line),$hostname group by $hostname outfile %s", csvFile)
	cleanupFiles(t, csvFile, queryFile)

	runCtx, runCancel := context.WithTimeout(server.ctx, 30*time.Second)
	defer runCancel()
	outFile := fmt.Sprintf("djournal_extended_dmap_%s.stdout.tmp", mode.name)
	args := []string{
		"--cfg", "none",
		"--logLevel", "error",
		"--noColor",
		"--query", query,
		"--servers", server.address,
		"--trustAllHosts",
		"--files", journalExtendedSpec,
	}
	if mode.expectTurbo {
		cmd, stdoutCh, stderrCh, cmdErrCh := startDJournalCommand(runCtx, t, nil, "../dmap", args...)
		assertDMapTurboLogState(t, server.logs, mode)
		got := waitFileContains(runCtx, t, csvFile, "3,integrationtest")
		drainCommandOutput(t, stdoutCh, stderrCh)
		stopProcessAndWait(t, cmd, cmdErrCh, "dmap journal turbo")
		if !strings.Contains(got, "count($line),$hostname") {
			t.Fatalf("dmap journal csv missing header:\n%s", got)
		}
		if err := verifyQueryFile(t, queryFile, query); err != nil {
			t.Fatal(err)
		}
		return got
	}

	_, err := runCommand(runCtx, t, outFile, "../dmap", args...)
	if err != nil {
		t.Fatalf("dmap journal failed: %v\nserver logs:\n%s", err, server.logs.String())
	}

	got := readTestFile(t, csvFile)
	if !strings.Contains(got, "count($line),$hostname") || !strings.Contains(got, "3,integrationtest") {
		t.Fatalf("dmap journal csv mismatch:\n%s", got)
	}
	if err := verifyQueryFile(t, queryFile, query); err != nil {
		t.Fatal(err)
	}
	assertDMapTurboLogState(t, server.logs, mode)
	return got
}

type dJournalExtendedServerMode struct {
	name               string
	env                map[string]string
	dropIntegrationEnv bool
	expectTurbo        bool
}

func dJournalExtendedServerModes() []dJournalExtendedServerMode {
	// Turbo is now the one and only runtime path, so the former two-mode
	// comparison collapses to a single turbo-enabled mode. All per-mode
	// golden/CSV assertions are preserved; only the redundant disabled iteration
	// (and its byte-identity comparison) is dropped.
	return []dJournalExtendedServerMode{
		{
			name: "turbo-enabled",
			env: map[string]string{
				"DTAIL_HOSTNAME_OVERRIDE": "integrationtest",
			},
			dropIntegrationEnv: true,
			expectTurbo:        true,
		},
	}
}

type dJournalExtendedEnv struct {
	configFile string
	mock       *journaltest.Mock
}

func newDJournalExtendedEnv(t *testing.T, scenario journaltest.Scenario,
	permissions []string, serverFields map[string]any) dJournalExtendedEnv {

	t.Helper()

	tmpDir := t.TempDir()
	mock := journaltest.InstallMock(t, scenario)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get test working directory: %v", err)
	}
	cacheDirAbs, err := os.MkdirTemp(cwd, "djournal-key-cache-")
	if err != nil {
		t.Fatalf("create journal extended key cache: %v", err)
	}
	t.Cleanup(func() {
		os.RemoveAll(cacheDirAbs)
	})
	if err := os.WriteFile(filepath.Join(cacheDirAbs, currentUsername(t)+".authorized_keys"),
		readIntegrationPublicKey(t), 0o600); err != nil {
		t.Fatalf("write journal extended authorized_keys cache: %v", err)
	}
	server := map[string]any{
		"HostKeyFile": filepath.Join(tmpDir, "ssh_host_key"),
		"Permissions": map[string]any{
			"Default": permissions,
		},
	}
	for key, value := range serverFields {
		server[key] = value
	}

	content, err := json.MarshalIndent(map[string]any{
		"Common": map[string]any{
			"CacheDir": filepath.Base(cacheDirAbs),
		},
		"Server": server,
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal journal extended config: %v", err)
	}

	configFile := filepath.Join(tmpDir, "dtail.json")
	if err := os.WriteFile(configFile, append(content, '\n'), 0o600); err != nil {
		t.Fatalf("write journal extended config: %v", err)
	}
	return dJournalExtendedEnv{
		configFile: configFile,
		mock:       mock,
	}
}

func currentUsername(t *testing.T) string {
	t.Helper()

	user, err := osuser.Current()
	if err != nil {
		t.Fatalf("get current user: %v", err)
	}
	return user.Username
}

func readIntegrationPublicKey(t *testing.T) []byte {
	t.Helper()

	path := config.IntegrationSSHPrivateKeyPath() + ".pub"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read integration public key %s: %v", path, err)
	}
	return data
}

type dJournalExtendedServer struct {
	ctx     context.Context
	address string
	logs    *safeLineLog
}

func startDJournalExtendedServer(t *testing.T, logger *TestLogger, configFile string,
	env map[string]string, logLevel string, dropIntegrationEnv bool) dJournalExtendedServer {

	t.Helper()

	port := getUniquePortNumber()
	bindAddress := "localhost"
	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTestLogger(ctx, logger)
	logs := &safeLineLog{}
	cmd := exec.CommandContext(ctx, "../dserver",
		"--cfg", configFile,
		"--logger", "stdout",
		"--logLevel", logLevel,
		"--bindAddress", bindAddress,
		"--port", fmt.Sprintf("%d", port),
	)
	cmd.Env = mergedCommandEnv(env, dropIntegrationEnv)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("open dserver stdout: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		t.Fatalf("open dserver stderr: %v", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start dserver: %v", err)
	}
	go scanToLog(stdout, logs)
	go scanToLog(stderr, logs)

	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Wait()
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-errCh
		}
	})

	if err := waitForServerReady(ctx, bindAddress, port); err != nil {
		cancel()
		t.Fatalf("wait for dserver: %v\nlogs:\n%s", err, logs.String())
	}

	return dJournalExtendedServer{
		ctx:     ctx,
		address: fmt.Sprintf("%s:%d", bindAddress, port),
		logs:    logs,
	}
}

func startDJournalExtendedServerForMode(t *testing.T, logger *TestLogger,
	env dJournalExtendedEnv, logLevel string, mode dJournalExtendedServerMode) dJournalExtendedServer {

	t.Helper()

	serverEnv := env.mock.Env()
	for key, value := range mode.env {
		serverEnv[key] = value
	}
	return startDJournalExtendedServer(t, logger, env.configFile, serverEnv, logLevel, mode.dropIntegrationEnv)
}

type safeLineLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *safeLineLog) add(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
}

func (l *safeLineLog) contains(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func (l *safeLineLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func (l *safeLineLog) waitContains(t *testing.T, substr string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		if l.contains(substr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server logs did not contain %q before timeout; logs:\n%s", substr, l.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func scanToLog(r io.Reader, logs *safeLineLog) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		logs.add(scanner.Text())
	}
}

func mergedCommandEnv(overrides map[string]string, dropIntegrationEnv bool) []string {
	skip := make(map[string]struct{}, len(overrides)+2)
	for key := range overrides {
		skip[key] = struct{}{}
	}
	if dropIntegrationEnv {
		skip["DTAIL_INTEGRATION_TEST_RUN_MODE"] = struct{}{}
		skip["DTAIL_TURBOBOOST_DISABLE"] = struct{}{}
	}

	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, item := range os.Environ() {
		key, _, found := strings.Cut(item, "=")
		if found {
			if _, ok := skip[key]; ok {
				continue
			}
		}
		env = append(env, item)
	}
	for key, value := range overrides {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}
	return env
}

func permissionForPath(path string) string {
	return "readfiles:^" + regexp.QuoteMeta(path) + "$"
}

func appendLinesAfterDelay(ctx context.Context, t *testing.T, path string,
	lines []string, delay time.Duration) {

	t.Helper()

	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Errorf("open %s for append: %v", path, err)
			return
		}
		defer file.Close()
		for _, line := range lines {
			if _, err := fmt.Fprintln(file, line); err != nil {
				t.Errorf("append to %s: %v", path, err)
				return
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

func readMatchingLines(ctx context.Context, t *testing.T, stdoutCh, stderrCh <-chan string,
	count int, match func(string) bool) []string {

	t.Helper()

	lines := make([]string, 0, count)
	for len(lines) < count {
		select {
		case line, ok := <-stdoutCh:
			if !ok {
				t.Fatalf("stdout closed after %d/%d matching lines: %v", len(lines), count, lines)
			}
			if match(line) {
				lines = append(lines, line)
			}
		case line, ok := <-stderrCh:
			if !ok {
				stderrCh = nil
				continue
			}
			if line != "" {
				t.Log("client stderr:", line)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for matching lines: %v; got %v", ctx.Err(), lines)
		}
	}
	return lines
}

func readMixedSourceLines(ctx context.Context, t *testing.T,
	stdoutCh, stderrCh <-chan string, logs *safeLineLog) []string {

	t.Helper()

	var lines []string
	for {
		if mixedSourcesInterleaved(lines) {
			return lines
		}

		select {
		case line, ok := <-stdoutCh:
			if !ok {
				t.Fatalf("stdout closed before mixed sources appeared: %v", lines)
			}
			if strings.Contains(line, "mixed") {
				lines = append(lines, line)
			}
		case line, ok := <-stderrCh:
			if !ok {
				stderrCh = nil
				continue
			}
			if line != "" {
				t.Log("client stderr:", line)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for mixed source output: %v; got %v\nserver logs:\n%s",
				ctx.Err(), lines, logs.String())
		}
	}
}

func waitFileContains(ctx context.Context, t *testing.T, path, substr string) string {
	t.Helper()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var last string
	for {
		content, err := os.ReadFile(path)
		if err == nil {
			last = string(content)
			if strings.Contains(last, substr) {
				return last
			}
		} else if !os.IsNotExist(err) {
			t.Fatalf("read %s: %v", path, err)
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s to contain %q: %v; last content:\n%s",
				path, substr, ctx.Err(), last)
		}
	}
}

func drainCommandOutput(t *testing.T, stdoutCh, stderrCh <-chan string) {
	t.Helper()

	for {
		if stdoutCh == nil && stderrCh == nil {
			return
		}
		select {
		case line, ok := <-stdoutCh:
			if !ok {
				stdoutCh = nil
				continue
			}
			if line != "" {
				t.Log("client stdout:", line)
			}
		case line, ok := <-stderrCh:
			if !ok {
				stderrCh = nil
				continue
			}
			if line != "" {
				t.Log("client stderr:", line)
			}
		default:
			return
		}
	}
}

func assertJournalTurboLogState(t *testing.T, logs *safeLineLog, mode dJournalExtendedServerMode) {
	t.Helper()

	// Turbo is now the one and only runtime path, so only the positive assertion
	// remains: the former negative (turbo-absent) branch is dead and was deleted.
	if mode.expectTurbo {
		logs.waitContains(t, "Using turbo mode for reading", 3*time.Second)
	}
}

func assertDMapTurboLogState(t *testing.T, logs *safeLineLog, mode dJournalExtendedServerMode) {
	t.Helper()

	// Turbo is now the one and only runtime path, so only the positive assertion
	// remains: the former negative (turbo-absent) branch is dead and was deleted.
	if mode.expectTurbo {
		logs.waitContains(t, "Creating turbo aggregate for MapReduce", 3*time.Second)
		logs.waitContains(t, "Using turbo aggregate processor for MapReduce", 3*time.Second)
	}
}

func assertMixedSourcesInterleaved(t *testing.T, lines []string) {
	t.Helper()

	if mixedSourcesInterleaved(lines) {
		return
	}
	t.Fatalf("mixed source output was not sufficiently interleaved: file lines=%d journal lines=%d switches=%d lines=%v",
		countLinePrefix(lines, "file mixed "), countLinePrefix(lines, "journal mixed "),
		sourceSwitchCount(lines), lines)
}

func mixedSourcesInterleaved(lines []string) bool {
	return countLinePrefix(lines, "file mixed ") >= mixedSourceMinLinesPerSource &&
		countLinePrefix(lines, "journal mixed ") >= mixedSourceMinLinesPerSource &&
		sourceSwitchCount(lines) >= mixedSourceMinSwitches
}

func countLinePrefix(lines []string, prefix string) int {
	count := 0
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			count++
		}
	}
	return count
}

func sourceSwitchCount(lines []string) int {
	lastSource := ""
	switches := 0
	for _, line := range lines {
		source := ""
		switch {
		case strings.HasPrefix(line, "file mixed "):
			source = "file"
		case strings.HasPrefix(line, "journal mixed "):
			source = "journal"
		}
		if source == "" {
			continue
		}
		if lastSource != "" && source != lastSource {
			switches++
		}
		lastSource = source
	}
	return switches
}

func stopProcessAndWait(t *testing.T, cmd *exec.Cmd, cmdErrCh <-chan error, name string) {
	t.Helper()

	select {
	case err := <-cmdErrCh:
		if err != nil {
			t.Fatalf("%s did not terminate cleanly: %v", name, err)
		}
		return
	default:
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("signal %s: %v", name, err)
	}
	select {
	case err := <-cmdErrCh:
		if err != nil {
			t.Fatalf("%s did not terminate cleanly after SIGTERM: %v", name, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not terminate after SIGTERM", name)
	}
}

func nonEmptyLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func readMockPID(t *testing.T, path string) int {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read mock pid file: %v", err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil {
		t.Fatalf("parse mock pid: %v", err)
	}
	return pid
}

func waitForProcessExit(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		if !processExists(pid) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d still exists after %s", pid, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

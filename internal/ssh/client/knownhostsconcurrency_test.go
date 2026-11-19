package client

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	knownHostsHelperPathEnv   = "DTAIL_TEST_KNOWN_HOSTS_HELPER_PATH"
	knownHostsHelperWriterEnv = "DTAIL_TEST_KNOWN_HOSTS_HELPER_WRITER"
	knownHostsHelperHosts     = 8
)

// TestTrustHostsConcurrentWriters runs many independent callbacks (as separate
// dtail clients would) against one known_hosts file at the same time. Every
// update must succeed, every entry must survive and no temporary file may be
// left behind.
func TestTrustHostsConcurrentWriters(t *testing.T) {
	const writers = 32
	dir := t.TempDir()
	knownHostsPath := filepath.Join(dir, "known_hosts")
	keepLine := knownhosts.Line([]string{"keep.example:2222"}, &mockPublicKey{id: "keep"})
	if err := os.WriteFile(knownHostsPath, []byte(keepLine+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	hosts := make([]unknownHost, writers)
	for i := range hosts {
		hosts[i] = concurrentTestHost(0, i)
	}

	var wg sync.WaitGroup
	errs := make([]error, writers)
	unlocked := make([]error, writers)
	start := make(chan struct{})
	for i := range hosts {
		callback := testKnownHostsCallback(t, knownHostsPath)
		wg.Go(func() {
			<-start
			unlocked[i], errs[i] = callback.trustHosts([]unknownHost{hosts[i]})
		})
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: trustHosts failed: %v", i, err)
		}
		if fileLockSupported && unlocked[i] != nil {
			t.Errorf("writer %d: updated without the lock: %v", i, unlocked[i])
		}
	}
	wantLines := []string{keepLine}
	for _, host := range hosts {
		wantLines = append(wantLines, host.hostLine, host.ipLine)
	}
	assertKnownHostsLines(t, knownHostsPath, wantLines)
	assertOnlyKnownHostsFiles(t, dir)
}

// TestTrustHostsConcurrentProcesses re-executes the test binary several times
// so that separate processes update the same known_hosts file concurrently.
func TestTrustHostsConcurrentProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns helper processes")
	}
	const processes = 6
	dir := t.TempDir()
	knownHostsPath := filepath.Join(dir, "known_hosts")

	cmds := make([]*exec.Cmd, processes)
	outputs := make([]strings.Builder, processes)
	for i := range cmds {
		cmd := exec.Command(os.Args[0], "-test.run=^TestKnownHostsWriterHelperProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(),
			knownHostsHelperPathEnv+"="+knownHostsPath,
			knownHostsHelperWriterEnv+"="+strconv.Itoa(i+1))
		cmd.Stdout = &outputs[i]
		cmd.Stderr = &outputs[i]
		cmds[i] = cmd
	}
	for i, cmd := range cmds {
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper %d: %v", i, err)
		}
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Errorf("helper %d failed: %v\n%s", i, err, outputs[i].String())
		}
	}

	var wantLines []string
	for writer := 1; writer <= processes; writer++ {
		for i := range knownHostsHelperHosts {
			host := concurrentTestHost(writer, i)
			wantLines = append(wantLines, host.hostLine, host.ipLine)
		}
	}
	assertKnownHostsLines(t, knownHostsPath, wantLines)
	assertOnlyKnownHostsFiles(t, dir)
}

// TestKnownHostsWriterHelperProcess is the child side of
// TestTrustHostsConcurrentProcesses; it does nothing in a normal test run.
func TestKnownHostsWriterHelperProcess(t *testing.T) {
	knownHostsPath := os.Getenv(knownHostsHelperPathEnv)
	if knownHostsPath == "" {
		t.Skip("helper process only")
	}
	writer, err := strconv.Atoi(os.Getenv(knownHostsHelperWriterEnv))
	if err != nil {
		t.Fatalf("parse writer index: %v", err)
	}

	var wg sync.WaitGroup
	for i := range knownHostsHelperHosts {
		callback := testKnownHostsCallback(t, knownHostsPath)
		wg.Go(func() {
			if _, err := callback.trustHosts([]unknownHost{concurrentTestHost(writer, i)}); err != nil {
				t.Errorf("writer %d host %d: %v", writer, i, err)
			}
		})
	}
	wg.Wait()
}

// TestTrustHostsSingleWriterLeavesNoTempFiles checks the content, the file
// mode and the directory contents after one ordinary update.
func TestTrustHostsSingleWriterLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	knownHostsPath := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHostsPath, nil, 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	callback := testKnownHostsCallback(t, knownHostsPath)
	host := concurrentTestHost(0, 1)
	if _, err := callback.trustHosts([]unknownHost{host}); err != nil {
		t.Fatalf("trustHosts failed: %v", err)
	}

	got, err := os.ReadFile(knownHostsPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if want := host.hostLine + "\n" + host.ipLine + "\n"; string(got) != want {
		t.Fatalf("known_hosts = %q, want %q", got, want)
	}
	info, err := os.Stat(knownHostsPath)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("known_hosts mode = %o, want 600", mode)
	}
	assertOnlyKnownHostsFiles(t, dir)
}

// TestTrustHostsErrorPathsLeaveNoTempFiles covers failures before and after
// the temporary file exists; neither may leave one behind or touch the file.
func TestTrustHostsErrorPathsLeaveNoTempFiles(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir, knownHostsPath string)
	}{
		{
			name: "unwritable directory",
			setup: func(t *testing.T, dir, knownHostsPath string) {
				if os.Geteuid() == 0 {
					t.Skip("root ignores directory permissions")
				}
				if err := os.WriteFile(knownHostsPath, []byte("keep\n"), 0o600); err != nil {
					t.Fatalf("WriteFile failed: %v", err)
				}
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatalf("Chmod failed: %v", err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			},
		},
		{
			name: "known_hosts is a directory",
			setup: func(t *testing.T, _, knownHostsPath string) {
				if err := os.Mkdir(knownHostsPath, 0o700); err != nil {
					t.Fatalf("Mkdir failed: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			knownHostsPath := filepath.Join(dir, "known_hosts")
			test.setup(t, dir, knownHostsPath)
			before := directoryEntries(t, dir)

			callback := testKnownHostsCallback(t, knownHostsPath)
			if _, err := callback.trustHosts([]unknownHost{concurrentTestHost(0, 1)}); err == nil {
				t.Fatal("trustHosts succeeded, want an error")
			}
			after := slices.DeleteFunc(directoryEntries(t, dir), func(name string) bool {
				return name == "known_hosts.lock"
			})
			if !slices.Equal(before, after) {
				t.Fatalf("directory entries changed from %v to %v", before, after)
			}
			if info, err := os.Stat(knownHostsPath); err == nil && info.Mode().IsRegular() {
				got, readErr := os.ReadFile(knownHostsPath)
				if readErr != nil || string(got) != "keep\n" {
					t.Fatalf("known_hosts changed to %q (%v)", got, readErr)
				}
			}
		})
	}
}

// TestLockKnownHostsTimesOutWhileHeld checks that a second lock attempt waits
// for the holder, gives up after the timeout and succeeds once released.
func TestLockKnownHostsTimesOutWhileHeld(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot failed: %v", err)
	}
	defer func() { _ = root.Close() }()

	release, err := lockKnownHosts(root, "known_hosts", time.Second)
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip("no advisory file locking on this platform")
	}
	if err != nil {
		t.Fatalf("first lock failed: %v", err)
	}
	started := time.Now()
	_, err = lockKnownHosts(root, "known_hosts", 50*time.Millisecond)
	if !errors.Is(err, errKnownHostsLockTimeout) {
		t.Fatalf("second lock error = %v, want %v", err, errKnownHostsLockTimeout)
	}
	if waited := time.Since(started); waited < 50*time.Millisecond {
		t.Fatalf("second lock gave up after %v, before the timeout", waited)
	}

	release()
	releaseAgain, err := lockKnownHosts(root, "known_hosts", time.Second)
	if err != nil {
		t.Fatalf("lock after release failed: %v", err)
	}
	releaseAgain()
}

func concurrentTestHost(writer, index int) unknownHost {
	key := &mockPublicKey{id: fmt.Sprintf("key-%d-%d", writer, index)}
	server := fmt.Sprintf("host-%d-%d.example:2222", writer, index)
	remote := &net.TCPAddr{IP: net.IPv4(10, 0, byte(writer), byte(index+1)), Port: 2222}
	return unknownHost{
		server:     server,
		remote:     remote,
		key:        key,
		hostLine:   knownhosts.Line([]string{server}, key),
		ipLine:     knownhosts.Line([]string{remote.String()}, key),
		responseCh: make(chan response, 1),
	}
}

func assertKnownHostsLines(t *testing.T, knownHostsPath string, wantLines []string) {
	t.Helper()
	data, err := os.ReadFile(knownHostsPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	gotLines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	slices.Sort(gotLines)
	want := slices.Clone(wantLines)
	slices.Sort(want)
	if !slices.Equal(gotLines, want) {
		missing := slices.DeleteFunc(slices.Clone(want), func(line string) bool {
			return slices.Contains(gotLines, line)
		})
		t.Fatalf("known_hosts has %d lines, want %d; missing %d: %v",
			len(gotLines), len(want), len(missing), missing)
	}
}

// assertOnlyKnownHostsFiles fails when anything besides the known_hosts file
// and its persistent lock file is left in dir.
func assertOnlyKnownHostsFiles(t *testing.T, dir string) {
	t.Helper()
	for _, name := range directoryEntries(t, dir) {
		if name != "known_hosts" && name != "known_hosts.lock" {
			t.Errorf("unexpected file left in known_hosts directory: %s", name)
		}
	}
}

func directoryEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// TestTrustHostsWarnsWhenLockIsHeld holds the known_hosts lock while a client
// updates the file: the update must go ahead after the lock timeout and log a
// warning, after the prompt has resumed logging in the interactive case.
func TestTrustHostsWarnsWhenLockIsHeld(t *testing.T) {
	if !fileLockSupported {
		t.Skip("no advisory file locking on this platform")
	}
	tests := []struct {
		name     string
		trustAll bool
	}{
		{name: "prompt answer yes"},
		{name: "trust all hosts", trustAll: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			knownHostsPath := filepath.Join(dir, "known_hosts")
			if err := os.WriteFile(knownHostsPath, nil, 0o600); err != nil {
				t.Fatalf("WriteFile failed: %v", err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatalf("OpenRoot failed: %v", err)
			}
			defer func() { _ = root.Close() }()
			release, err := lockKnownHosts(root, "known_hosts", time.Second)
			if err != nil {
				t.Fatalf("holding the lock failed: %v", err)
			}
			defer release()

			logger := &recordingLogger{}
			callback := testKnownHostsCallbackWithLogger(t, knownHostsPath, logger)
			callback.lockTimeout = 20 * time.Millisecond
			host := concurrentTestHost(0, 1)
			started := time.Now()
			if test.trustAll {
				callback.closeTrustAllHostsCh()
				callback.promptAddHosts([]unknownHost{host})
			} else {
				withPromptInput(t, "yes\n", func() {
					callback.promptAddHosts([]unknownHost{host})
				})
			}
			if waited := time.Since(started); waited >= knownHostsLockTimeout {
				t.Fatalf("update waited %v, not the injected lock timeout", waited)
			}

			if got := logger.warnCount.Load(); got != 1 {
				t.Fatalf("Warn calls = %d, want 1", got)
			}
			if warned := logger.lastWarnValue(); !errors.Is(warned, errKnownHostsLockTimeout) {
				t.Fatalf("warning error = %v, want %v", warned, errKnownHostsLockTimeout)
			}
			if logger.logWhilePaused.Load() {
				t.Fatal("lock warning logged while the prompt paused logging")
			}
			if got := logger.errorCount.Load(); got != 0 {
				t.Fatalf("Error calls = %d, want 0", got)
			}
			assertKnownHostsLines(t, knownHostsPath, []string{host.hostLine, host.ipLine})
		})
	}
}

// TestLockKnownHostsUnsupportedCreatesNoLockFile checks that a platform
// without advisory locking gets no useless lock file and that the update
// still succeeds there without a warning.
func TestLockKnownHostsUnsupportedCreatesNoLockFile(t *testing.T) {
	previous := fileLockSupported
	fileLockSupported = false
	t.Cleanup(func() { fileLockSupported = previous })

	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot failed: %v", err)
	}
	defer func() { _ = root.Close() }()
	release, err := lockKnownHosts(root, "known_hosts", time.Second)
	release()
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("lock error = %v, want %v", err, errors.ErrUnsupported)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "known_hosts.lock")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("lock file stat = %v, want it not to exist", statErr)
	}

	knownHostsPath := filepath.Join(dir, "known_hosts")
	logger := &recordingLogger{}
	callback := testKnownHostsCallbackWithLogger(t, knownHostsPath, logger)
	callback.closeTrustAllHostsCh()
	host := concurrentTestHost(0, 1)
	callback.promptAddHosts([]unknownHost{host})
	if got := logger.warnCount.Load(); got != 0 {
		t.Fatalf("Warn calls = %d, want 0 on a platform without locking", got)
	}
	assertKnownHostsLines(t, knownHostsPath, []string{host.hostLine, host.ipLine})
	if _, statErr := os.Stat(filepath.Join(dir, "known_hosts.lock")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("lock file stat after update = %v, want it not to exist", statErr)
	}
}

// TestLockKnownHostsReadOnlyLockFile checks that a lock file the user can
// only read (as another user's shared lock file would be) still locks and
// still excludes a second holder.
func TestLockKnownHostsReadOnlyLockFile(t *testing.T) {
	if !fileLockSupported {
		t.Skip("no advisory file locking on this platform")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "known_hosts.lock"), nil, 0o400); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot failed: %v", err)
	}
	defer func() { _ = root.Close() }()

	release, err := lockKnownHosts(root, "known_hosts", time.Second)
	if err != nil {
		t.Fatalf("lock of read-only lock file failed: %v", err)
	}
	defer release()
	if _, err := lockKnownHosts(root, "known_hosts", 20*time.Millisecond); !errors.Is(err, errKnownHostsLockTimeout) {
		t.Fatalf("second lock error = %v, want %v", err, errKnownHostsLockTimeout)
	}
}

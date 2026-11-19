package integrationtests

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
	dtailssh "github.com/mimecast/dtail/internal/ssh"
	gossh "golang.org/x/crypto/ssh"
)

const (
	authKeyFastPathLog      = "Authorized by in-memory auth key store"
	dcatExpectedFirstOutput = "1 Sat  2 Oct 13:46:45 EEST 2021"
)

type suiteAuthKey struct {
	privateKeyPath string
	tempDir        string
}

func TestMain(m *testing.M) {
	var authKey suiteAuthKey
	if config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		var err error
		authKey, err = prepareSuiteAuthKey(generateAuthKeyPair)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Unable to prepare integration SSH key pair: %v\n", err)
			os.Exit(1)
		}
	}

	exitCode := m.Run()
	if err := authKey.cleanup(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Unable to remove generated integration SSH key pair: %v\n", err)
		exitCode = 1
	}
	os.Exit(exitCode)
}

func TestAuthKeyFastReconnectIntegration(t *testing.T) {
	skipIfNotIntegrationTest(t)
	cleanupTmpFiles(t)

	t.Run("RegistrationFastPathAndFallback", testAuthKeyRegistrationFastPathAndFallback)
	t.Run("TTLExpiry", testAuthKeyTTLExpiry)
	t.Run("MaxKeysPerUser", testAuthKeyMaxKeysPerUser)
	t.Run("NoAuthKeyFlag", testNoAuthKeyFlagDisablesFeature)
	t.Run("PassphraseProtectedKey", testPassphraseKeyAuthKeyRegistrationAndFastReconnect)
}

func TestPrepareSuiteAuthKeyCreatesPrivateTemporaryPair(t *testing.T) {
	t.Setenv("DTAIL_AUTH_KEY_PATH", "")

	authKey, err := prepareSuiteAuthKey(generateAuthKeyPair)
	if err != nil {
		t.Fatalf("prepare suite auth key: %v", err)
	}
	if authKey.tempDir == "" || filepath.Dir(authKey.privateKeyPath) != authKey.tempDir {
		t.Fatalf("Expected process-private key directory, got %#v", authKey)
	}
	if got := os.Getenv("DTAIL_AUTH_KEY_PATH"); got != authKey.privateKeyPath {
		t.Fatalf("DTAIL_AUTH_KEY_PATH = %q, want %q", got, authKey.privateKeyPath)
	}
	assertFileMode(t, authKey.tempDir, 0700)
	assertFileMode(t, authKey.privateKeyPath, 0600)
	assertFileMode(t, authKey.privateKeyPath+".pub", 0600)
	if err := validateAuthKeyPair(authKey.privateKeyPath); err != nil {
		t.Fatalf("generated suite auth key is invalid: %v", err)
	}

	if err := authKey.cleanup(); err != nil {
		t.Fatalf("clean up suite auth key: %v", err)
	}
	assertAuthKeyPathMissing(t, authKey.tempDir)
	if got := os.Getenv("DTAIL_AUTH_KEY_PATH"); got != "" {
		t.Fatalf("DTAIL_AUTH_KEY_PATH remained %q after cleanup", got)
	}
}

func TestPrepareSuiteAuthKeyPreservesConfiguredPair(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "configured-id_rsa")
	createAuthKeyPairAtPath(t, keyPath)
	wantPrivate, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read configured private key: %v", err)
	}
	wantPublic, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatalf("read configured public key: %v", err)
	}
	t.Setenv("DTAIL_AUTH_KEY_PATH", keyPath)

	authKey, err := prepareSuiteAuthKey(func(string) error {
		return errors.New("generator must not be called for configured key")
	})
	if err != nil {
		t.Fatalf("prepare configured suite auth key: %v", err)
	}
	if authKey.privateKeyPath != keyPath || authKey.tempDir != "" {
		t.Fatalf("Expected unowned configured key, got %#v", authKey)
	}
	if err := authKey.cleanup(); err != nil {
		t.Fatalf("clean up configured suite auth key: %v", err)
	}
	assertFileContents(t, keyPath, wantPrivate)
	assertFileContents(t, keyPath+".pub", wantPublic)
	if got := os.Getenv("DTAIL_AUTH_KEY_PATH"); got != keyPath {
		t.Fatalf("Configured DTAIL_AUTH_KEY_PATH changed to %q", got)
	}
}

func TestPrepareSuiteAuthKeyRejectsInvalidConfiguredPair(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{name: "Missing", setup: func(*testing.T, string) {}},
		{name: "PrivateOnly", setup: func(t *testing.T, path string) {
			writeTestFile(t, path, "not a private key")
		}},
		{name: "PublicOnly", setup: func(t *testing.T, path string) {
			writeTestFile(t, path+".pub", "not a public key")
		}},
		{name: "InvalidPrivate", setup: func(t *testing.T, path string) {
			createAuthKeyPairAtPath(t, path)
			writeTestFile(t, path, "not a private key")
		}},
		{name: "InvalidPublic", setup: func(t *testing.T, path string) {
			createAuthKeyPairAtPath(t, path)
			writeTestFile(t, path+".pub", "not a public key")
		}},
		{name: "Mismatched", setup: func(t *testing.T, path string) {
			createAuthKeyPairAtPath(t, path)
			otherPath := filepath.Join(t.TempDir(), "other-id_rsa")
			createAuthKeyPairAtPath(t, otherPath)
			otherPublic, err := os.ReadFile(otherPath + ".pub")
			if err != nil {
				t.Fatalf("read other public key: %v", err)
			}
			if err := os.WriteFile(path+".pub", otherPublic, 0600); err != nil {
				t.Fatalf("replace public key: %v", err)
			}
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			keyPath := filepath.Join(t.TempDir(), "configured-id_rsa")
			test.setup(t, keyPath)
			t.Setenv("DTAIL_AUTH_KEY_PATH", keyPath)

			authKey, err := prepareSuiteAuthKey(func(string) error {
				return errors.New("generator must not be called for configured key")
			})
			if err == nil || !strings.Contains(err.Error(), "configured integration SSH key pair") {
				t.Fatalf("Expected configured-pair validation error, got key=%#v err=%v", authKey, err)
			}
			if authKey.tempDir != "" {
				t.Fatalf("Invalid configured key unexpectedly became owned: %#v", authKey)
			}
		})
	}
}

func TestPrepareSuiteAuthKeyCleansPartialGenerationFailure(t *testing.T) {
	t.Setenv("DTAIL_AUTH_KEY_PATH", "")
	var generatedPath string

	authKey, err := prepareSuiteAuthKey(func(keyPath string) error {
		generatedPath = keyPath
		if writeErr := os.WriteFile(keyPath, []byte("partial private key"), 0600); writeErr != nil {
			return writeErr
		}
		return errors.New("forced key-generation write failure")
	})
	if err == nil || !strings.Contains(err.Error(), "forced key-generation write failure") {
		t.Fatalf("Expected generation failure, got key=%#v err=%v", authKey, err)
	}
	if generatedPath == "" {
		t.Fatal("Expected generator to receive a private-key path")
	}
	assertAuthKeyPathMissing(t, filepath.Dir(generatedPath))
	if got := os.Getenv("DTAIL_AUTH_KEY_PATH"); got != "" {
		t.Fatalf("DTAIL_AUTH_KEY_PATH changed to %q after failed generation", got)
	}
}

func testAuthKeyRegistrationFastPathAndFallback(t *testing.T) {
	authKeyPath := createAuthKeyPair(t, "authkey-registration")
	server := startAuthKeyServer(t, "")
	defer server.Stop()

	exitCode, err := runDCatWithAuthKey(server.Context(), t, "authkey_registration_1.tmp", server.Address(), authKeyPath, false)
	if err != nil || exitCode != 0 {
		t.Fatalf("Expected first connection to succeed, exit=%d err=%v", exitCode, err)
	}
	assertDCatSuccessfulOutput(t, "authkey_registration_1.tmp")
	waitForServerLogs()
	if got := server.CountLogLinesContaining(authKeyFastPathLog); got != 0 {
		t.Fatalf("Expected first connection to use fallback, fast-path count=%d", got)
	}

	exitCode, err = runDCatWithAuthKey(server.Context(), t, "authkey_registration_2.tmp", server.Address(), authKeyPath, false)
	if err != nil || exitCode != 0 {
		t.Fatalf("Expected second connection to succeed, exit=%d err=%v", exitCode, err)
	}
	assertDCatSuccessfulOutput(t, "authkey_registration_2.tmp")
	waitForServerLogs()
	if got := server.CountLogLinesContaining(authKeyFastPathLog); got < 1 {
		t.Fatalf("Expected fast-path authorization after registration, fast-path count=%d", got)
	}

	server.Stop()
	time.Sleep(300 * time.Millisecond)

	restartedServer := startAuthKeyServer(t, "")
	defer restartedServer.Stop()

	exitCode, err = runDCatWithAuthKey(restartedServer.Context(), t, "authkey_registration_3.tmp", restartedServer.Address(), authKeyPath, false)
	if err != nil || exitCode != 0 {
		t.Fatalf("Expected fallback after restart to succeed, exit=%d err=%v", exitCode, err)
	}
	assertDCatSuccessfulOutput(t, "authkey_registration_3.tmp")
	waitForServerLogs()
	if got := restartedServer.CountLogLinesContaining(authKeyFastPathLog); got != 0 {
		t.Fatalf("Expected no fast-path hit on first post-restart connection, fast-path count=%d", got)
	}
}

func testAuthKeyTTLExpiry(t *testing.T) {
	authKeyPath := createAuthKeyPair(t, "authkey-ttl")
	ttlSeconds := 8
	cfgFile := writeAuthKeyServerConfig(t, ttlSeconds, 5)
	server := startAuthKeyServer(t, cfgFile)
	defer server.Stop()

	exitCode, err := runDCatWithAuthKey(server.Context(), t, "authkey_ttl_1.tmp", server.Address(), authKeyPath, false)
	if err != nil || exitCode != 0 {
		t.Fatalf("Expected first connection to succeed, exit=%d err=%v", exitCode, err)
	}
	assertDCatSuccessfulOutput(t, "authkey_ttl_1.tmp")

	exitCode, err = runDCatWithAuthKey(server.Context(), t, "authkey_ttl_2.tmp", server.Address(), authKeyPath, false)
	if err != nil || exitCode != 0 {
		t.Fatalf("Expected second connection to succeed, exit=%d err=%v", exitCode, err)
	}
	assertDCatSuccessfulOutput(t, "authkey_ttl_2.tmp")
	fastPathCountAfterSecond := waitForLogCountAtLeast(server, authKeyFastPathLog, 1, 5*time.Second)
	if fastPathCountAfterSecond < 1 {
		t.Fatalf("Expected fast-path hit before TTL expiry, count=%d\nserver logs:\n%s",
			fastPathCountAfterSecond, strings.Join(server.LogLines(), "\n"))
	}

	time.Sleep(time.Duration(ttlSeconds+1) * time.Second)
	exitCode, err = runDCatWithAuthKey(server.Context(), t, "authkey_ttl_3.tmp", server.Address(), authKeyPath, false)
	if err != nil || exitCode != 0 {
		t.Fatalf("Expected fallback after TTL expiry to still connect, exit=%d err=%v", exitCode, err)
	}
	assertDCatSuccessfulOutput(t, "authkey_ttl_3.tmp")
	waitForServerLogs()
	fastPathCountAfterThird := server.CountLogLinesContaining(authKeyFastPathLog)
	if fastPathCountAfterThird != fastPathCountAfterSecond {
		t.Fatalf("Expected TTL-expired key to stop fast-path hits: before=%d after=%d",
			fastPathCountAfterSecond, fastPathCountAfterThird)
	}
}

func testAuthKeyMaxKeysPerUser(t *testing.T) {
	authKeyOne := createAuthKeyPair(t, "authkey-max-one")
	authKeyTwo := createAuthKeyPair(t, "authkey-max-two")
	cfgFile := writeAuthKeyServerConfig(t, 3600, 1)
	server := startAuthKeyServer(t, cfgFile)
	defer server.Stop()

	if exitCode, err := runDCatWithAuthKey(server.Context(), t, "authkey_max_1.tmp", server.Address(), authKeyOne, false); err != nil || exitCode != 0 {
		t.Fatalf("Expected first key registration to succeed, exit=%d err=%v", exitCode, err)
	}
	assertDCatSuccessfulOutput(t, "authkey_max_1.tmp")
	if exitCode, err := runDCatWithAuthKey(server.Context(), t, "authkey_max_2.tmp", server.Address(), authKeyTwo, false); err != nil || exitCode != 0 {
		t.Fatalf("Expected second key registration to succeed, exit=%d err=%v", exitCode, err)
	}
	assertDCatSuccessfulOutput(t, "authkey_max_2.tmp")
	waitForServerLogs()
	initialFastPathCount := server.CountLogLinesContaining(authKeyFastPathLog)

	if exitCode, err := runDCatWithAuthKey(server.Context(), t, "authkey_max_3.tmp", server.Address(), authKeyOne, false); err != nil || exitCode != 0 {
		t.Fatalf("Expected first key connection (after max eviction) to succeed via fallback, exit=%d err=%v", exitCode, err)
	}
	assertDCatSuccessfulOutput(t, "authkey_max_3.tmp")
	waitForServerLogs()
	afterOldKeyCount := server.CountLogLinesContaining(authKeyFastPathLog)
	if afterOldKeyCount != initialFastPathCount {
		t.Fatalf("Expected evicted old key to avoid fast-path hit: before=%d after=%d",
			initialFastPathCount, afterOldKeyCount)
	}

	if exitCode, err := runDCatWithAuthKey(server.Context(), t, "authkey_max_4.tmp", server.Address(), authKeyOne, false); err != nil || exitCode != 0 {
		t.Fatalf("Expected re-registered first key to succeed, exit=%d err=%v", exitCode, err)
	}
	assertDCatSuccessfulOutput(t, "authkey_max_4.tmp")
	waitForServerLogs()
	afterNewKeyCount := server.CountLogLinesContaining(authKeyFastPathLog)
	if afterNewKeyCount <= afterOldKeyCount {
		t.Fatalf("Expected re-registered key to use fast-path: old-count=%d new-count=%d", afterOldKeyCount, afterNewKeyCount)
	}
}

func testNoAuthKeyFlagDisablesFeature(t *testing.T) {
	authKeyPath := createAuthKeyPair(t, "authkey-noauth")
	server := startAuthKeyServer(t, "")
	defer server.Stop()

	if exitCode, err := runDCatWithAuthKey(server.Context(), t, "authkey_noauth_1.tmp", server.Address(), authKeyPath, true); err != nil || exitCode != 0 {
		t.Fatalf("Expected first --no-auth-key connection to succeed, exit=%d err=%v", exitCode, err)
	}
	assertDCatSuccessfulOutput(t, "authkey_noauth_1.tmp")
	if exitCode, err := runDCatWithAuthKey(server.Context(), t, "authkey_noauth_2.tmp", server.Address(), authKeyPath, true); err != nil || exitCode != 0 {
		t.Fatalf("Expected second --no-auth-key connection to succeed, exit=%d err=%v", exitCode, err)
	}
	assertDCatSuccessfulOutput(t, "authkey_noauth_2.tmp")

	waitForServerLogs()
	if got := server.CountLogLinesContaining(authKeyFastPathLog); got != 0 {
		t.Fatalf("Expected --no-auth-key to prevent fast-path registration, fast-path count=%d", got)
	}
}

func testPassphraseKeyAuthKeyRegistrationAndFastReconnect(t *testing.T) {
	const (
		authKeyPassphrase = "secret-passphrase"
		suitePassphrase   = "different-suite-passphrase"
	)

	// Reproduce a caller-provided encrypted suite key whose passphrase differs
	// from the explicit key under test. The explicit key must bootstrap this
	// server because one DTAIL_KEY_PASSPHRASE cannot decrypt both keys.
	suiteAuthKeyPath := createPassphraseAuthKeyPair(t, "suite-authkey-passphrase", suitePassphrase)
	t.Setenv("DTAIL_AUTH_KEY_PATH", suiteAuthKeyPath)
	t.Setenv("DTAIL_KEY_PASSPHRASE", suitePassphrase)

	authKeyPath := createPassphraseAuthKeyPair(t, "authkey-passphrase", authKeyPassphrase)
	server := startAuthKeyServerWithEnv(t, "", map[string]string{
		"DTAIL_AUTH_KEY_PATH":  authKeyPath,
		"DTAIL_KEY_PASSPHRASE": authKeyPassphrase,
	})
	defer server.Stop()

	env := map[string]string{
		"DTAIL_KEY_PASSPHRASE": authKeyPassphrase,
	}

	exitCode, err := runDCatWithAuthKeyAndEnv(server.Context(), t,
		"authkey_passphrase_1.tmp", server.Address(), authKeyPath, false, env)
	if err != nil || exitCode != 0 {
		t.Fatalf("Expected first passphrase-protected connection to succeed, exit=%d err=%v", exitCode, err)
	}
	assertDCatSuccessfulOutput(t, "authkey_passphrase_1.tmp")
	waitForServerLogs()
	if got := server.CountLogLinesContaining(authKeyFastPathLog); got != 0 {
		t.Fatalf("Expected first passphrase-protected connection to use fallback, fast-path count=%d", got)
	}

	exitCode, err = runDCatWithAuthKeyAndEnv(server.Context(), t,
		"authkey_passphrase_2.tmp", server.Address(), authKeyPath, false, env)
	if err != nil || exitCode != 0 {
		t.Fatalf("Expected second passphrase-protected connection to succeed, exit=%d err=%v", exitCode, err)
	}
	assertDCatSuccessfulOutput(t, "authkey_passphrase_2.tmp")
	fastPathCount := waitForLogCountAtLeast(server, authKeyFastPathLog, 1, 5*time.Second)
	if fastPathCount < 1 {
		t.Fatalf("Expected passphrase-protected key to use fast-path on reconnect, count=%d\nserver logs:\n%s",
			fastPathCount, strings.Join(server.LogLines(), "\n"))
	}
}

type authKeyServer struct {
	ctx    context.Context
	cancel context.CancelFunc
	addr   string
	logs   *authKeyServerLogs
}

func (s *authKeyServer) Stop() {
	s.cancel()
}

func (s *authKeyServer) Context() context.Context {
	return s.ctx
}

func (s *authKeyServer) Address() string {
	return s.addr
}

func (s *authKeyServer) CountLogLinesContaining(substring string) int {
	return s.logs.countContaining(substring)
}

func (s *authKeyServer) LogLines() []string {
	return s.logs.snapshot()
}

type authKeyServerLogs struct {
	mu    sync.Mutex
	lines []string
}

func newAuthKeyServerLogs() *authKeyServerLogs {
	return &authKeyServerLogs{
		lines: make([]string, 0, 128),
	}
}

func (l *authKeyServerLogs) append(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
}

func (l *authKeyServerLogs) countContaining(substring string) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	count := 0
	for _, line := range l.lines {
		if strings.Contains(line, substring) {
			count++
		}
	}
	return count
}

func (l *authKeyServerLogs) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	lines := make([]string, len(l.lines))
	copy(lines, l.lines)
	return lines
}

func startAuthKeyServer(t *testing.T, cfgFile string) *authKeyServer {
	t.Helper()
	return startAuthKeyServerWithEnv(t, cfgFile, nil)
}

func startAuthKeyServerWithEnv(t *testing.T, cfgFile string,
	env map[string]string) *authKeyServer {

	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	port := getUniquePortNumber()
	args := []string{
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "info",
		"--bindAddress", "localhost",
		"--port", fmt.Sprintf("%d", port),
	}
	if cfgFile != "" {
		args = append(args, "--cfg", cfgFile)
	}

	serverEnv := map[string]string{"DTAIL_TURBOBOOST_DISABLE": "yes"}
	for name, value := range env {
		serverEnv[name] = value
	}
	stdoutCh, stderrCh, cmdErrCh, err := startCommandWithEnv(ctx, t, "", "../dserver",
		serverEnv, args...)
	if err != nil {
		cancel()
		t.Fatalf("Unable to start dserver: %v", err)
	}

	logs := newAuthKeyServerLogs()
	go func() {
		for {
			select {
			case line, ok := <-stdoutCh:
				if ok {
					logs.append(line)
				}
			case line, ok := <-stderrCh:
				if ok {
					logs.append(line)
				}
			case err := <-cmdErrCh:
				if err != nil {
					logs.append(err.Error())
				}
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	if err := waitForServerReady(ctx, "localhost", port); err != nil {
		cancel()
		t.Fatalf("Unable to start dserver: %v", err)
	}
	return &authKeyServer{
		ctx:    ctx,
		cancel: cancel,
		addr:   fmt.Sprintf("localhost:%d", port),
		logs:   logs,
	}
}

func runDCatWithAuthKey(ctx context.Context, t *testing.T, outFile,
	serverAddress, authKeyPath string, noAuthKey bool) (int, error) {
	return runDCatWithAuthKeyAndEnv(ctx, t, outFile, serverAddress, authKeyPath, noAuthKey, nil)
}

func runDCatWithAuthKeyAndEnv(ctx context.Context, t *testing.T, outFile,
	serverAddress, authKeyPath string, noAuthKey bool, env map[string]string) (int, error) {
	t.Helper()

	args := []string{
		"--plain",
		"--cfg", "none",
		"--servers", serverAddress,
		"--files", "dcat1a.txt",
		"--trustAllHosts",
		"--noColor",
		"--auth-key-path", authKeyPath,
	}
	if noAuthKey {
		args = append(args, "--no-auth-key")
	}

	return runCommandWithEnv(ctx, t, outFile, "../dcat", env, args...)
}

func assertDCatSuccessfulOutput(t *testing.T, outFile string) {
	t.Helper()

	outBytes, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("Unable to read dcat output file %s: %v", outFile, err)
	}

	output := string(outBytes)
	if strings.Contains(output, "SSH handshake failed") {
		t.Fatalf("Expected successful SSH connection, got handshake failure in %s:\n%s", outFile, output)
	}
	if !strings.Contains(output, dcatExpectedFirstOutput) {
		t.Fatalf("Expected dcat output to contain %q in %s, got:\n%s", dcatExpectedFirstOutput, outFile, output)
	}
}

func writeAuthKeyServerConfig(t *testing.T, ttlSeconds, maxPerUser int) string {
	t.Helper()

	cfgPath := filepath.Join(t.TempDir(), "authkey_server_config.json")
	cfgContent := fmt.Sprintf(
		`{"Server":{"AuthKeyEnabled":true,"AuthKeyTTLSeconds":%d,"AuthKeyMaxPerUser":%d}}`,
		ttlSeconds, maxPerUser,
	)
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0600); err != nil {
		t.Fatalf("Unable to write auth-key server config: %v", err)
	}
	return cfgPath
}

func prepareSuiteAuthKey(generateKeyPair func(string) error) (suiteAuthKey, error) {
	if configuredPath := os.Getenv("DTAIL_AUTH_KEY_PATH"); configuredPath != "" {
		if err := validateAuthKeyPair(configuredPath); err != nil {
			return suiteAuthKey{}, fmt.Errorf("validate configured integration SSH key pair: %w", err)
		}
		return suiteAuthKey{privateKeyPath: configuredPath}, nil
	}

	tempDir, err := os.MkdirTemp("", "dtail-integration-auth-key-")
	if err != nil {
		return suiteAuthKey{}, fmt.Errorf("create integration SSH key directory: %w", err)
	}
	keyPath := filepath.Join(tempDir, "id_rsa")
	if err := generateKeyPair(keyPath); err != nil {
		cleanupErr := os.RemoveAll(tempDir)
		return suiteAuthKey{}, errors.Join(
			fmt.Errorf("generate integration SSH key pair: %w", err),
			wrapTempDirCleanupError(tempDir, cleanupErr),
		)
	}
	if err := os.Setenv("DTAIL_AUTH_KEY_PATH", keyPath); err != nil {
		cleanupErr := os.RemoveAll(tempDir)
		return suiteAuthKey{}, errors.Join(
			fmt.Errorf("set DTAIL_AUTH_KEY_PATH: %w", err),
			wrapTempDirCleanupError(tempDir, cleanupErr),
		)
	}

	return suiteAuthKey{privateKeyPath: keyPath, tempDir: tempDir}, nil
}

func validateAuthKeyPair(keyPath string) error {
	for _, path := range []string{keyPath, keyPath + ".pub"} {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("integration SSH key %s is not a regular file (mode %s)", path, info.Mode())
		}
	}

	privateSigner, err := dtailssh.PrivateKeySigner(keyPath)
	if err != nil {
		return fmt.Errorf("parse private key %s: %w", keyPath, err)
	}
	publicKeyBytes, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return fmt.Errorf("read public key %s.pub: %w", keyPath, err)
	}
	publicKey, _, _, rest, err := gossh.ParseAuthorizedKey(publicKeyBytes)
	if err != nil {
		return fmt.Errorf("parse public key %s.pub: %w", keyPath, err)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return fmt.Errorf("public key %s.pub contains more than one key", keyPath)
	}
	if !bytes.Equal(privateSigner.PublicKey().Marshal(), publicKey.Marshal()) {
		return fmt.Errorf("private and public keys do not match for %s", keyPath)
	}
	return nil
}

func wrapTempDirCleanupError(path string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("remove integration SSH key directory %s: %w", path, err)
}

func (k suiteAuthKey) cleanup() error {
	if k.tempDir == "" {
		return nil
	}

	var unsetErr error
	if os.Getenv("DTAIL_AUTH_KEY_PATH") == k.privateKeyPath {
		if err := os.Unsetenv("DTAIL_AUTH_KEY_PATH"); err != nil {
			unsetErr = fmt.Errorf("unset DTAIL_AUTH_KEY_PATH: %w", err)
		}
	}
	removeErr := wrapTempDirCleanupError(k.tempDir, os.RemoveAll(k.tempDir))
	return errors.Join(unsetErr, removeErr)
}

func createAuthKeyPair(t *testing.T, keyName string) string {
	t.Helper()

	keyPath := filepath.Join(t.TempDir(), keyName)
	createAuthKeyPairAtPath(t, keyPath)
	return keyPath
}

func createAuthKeyPairAtPath(t *testing.T, keyPath string) {
	t.Helper()

	if err := generateAuthKeyPair(keyPath); err != nil {
		t.Fatalf("Unable to create auth-key pair: %v", err)
	}
}

func generateAuthKeyPair(keyPath string) error {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate private key: %w", err)
	}

	privateKeyBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})
	return writeAuthKeyPair(keyPath, privateKeyBytes, &privateKey.PublicKey)
}

func createPassphraseAuthKeyPair(t *testing.T, keyName, passphrase string) string {
	t.Helper()

	keyPath := filepath.Join(t.TempDir(), keyName)
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Unable to generate private key: %v", err)
	}

	privateKeyBlock, err := gossh.MarshalPrivateKeyWithPassphrase(privateKey, "", []byte(passphrase))
	if err != nil {
		t.Fatalf("Unable to marshal encrypted private key: %v", err)
	}
	if err := writeAuthKeyPair(keyPath, pem.EncodeToMemory(privateKeyBlock), &privateKey.PublicKey); err != nil {
		t.Fatalf("Unable to write auth-key pair: %v", err)
	}

	return keyPath
}

func writeAuthKeyPair(keyPath string, privateKeyBytes []byte,
	publicKey *rsa.PublicKey) error {
	sshPublicKey, err := gossh.NewPublicKey(publicKey)
	if err != nil {
		return fmt.Errorf("generate public key: %w", err)
	}
	return writeAuthKeyBytes(keyPath, privateKeyBytes, gossh.MarshalAuthorizedKey(sshPublicKey))
}

func writeAuthKeyBytes(keyPath string, privateKeyBytes,
	publicKeyBytes []byte) error {
	if err := os.WriteFile(keyPath, privateKeyBytes, 0600); err != nil {
		return fmt.Errorf("write private key %s: %w", keyPath, err)
	}
	if err := os.WriteFile(keyPath+".pub", publicKeyBytes, 0600); err != nil {
		return fmt.Errorf("write public key %s.pub: %w", keyPath, err)
	}
	return nil
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("Unable to write test file %s: %v", path, err)
	}
}

func assertFileMode(t *testing.T, path string, expected os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Unable to inspect %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != expected {
		t.Fatalf("Permissions for %s = %04o, want %04o", path, got, expected)
	}
}

func assertFileContents(t *testing.T, path string, expected []byte) {
	t.Helper()

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Unable to read %s: %v", path, err)
	}
	if !bytes.Equal(contents, expected) {
		t.Fatalf("Unexpected contents for %s", path)
	}
}

func assertAuthKeyPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("Expected auth-key path %s to be absent, got err=%v", path, err)
	}
}

func waitForServerLogs() {
	time.Sleep(300 * time.Millisecond)
}

func waitForLogCountAtLeast(server *authKeyServer, substring string, minCount int, timeout time.Duration) int {
	if minCount <= 0 {
		return server.CountLogLinesContaining(substring)
	}

	deadline := time.Now().Add(timeout)
	for {
		count := server.CountLogLinesContaining(substring)
		if count >= minCount {
			return count
		}
		if time.Now().After(deadline) {
			return count
		}
		time.Sleep(100 * time.Millisecond)
	}
}

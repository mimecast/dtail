package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/source"
)

func TestSetupReturnsConfigDecodeError(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "invalid.conf")
	writeTestConfig(t, configPath, `{not-json`)

	err := Setup(source.Client, &Args{ConfigFile: configPath}, nil)
	if err == nil {
		t.Fatal("Setup succeeded with invalid JSON")
	}
	for _, want := range []string{"load configuration", "parse config file", configPath} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Fatalf("Setup error %q does not contain %q", err, want)
		}
	}
}

func TestSetupRuntimeReturnsConfigWithoutPublishingGlobals(t *testing.T) {
	previousClient, previousServer, previousCommon := Client, Server, Common
	sentinelClient := &ClientConfig{}
	sentinelServer := &ServerConfig{}
	sentinelCommon := &CommonConfig{HostnameOverride: "legacy-host"}
	Client, Server, Common = sentinelClient, sentinelServer, sentinelCommon
	t.Cleanup(func() { Client, Server, Common = previousClient, previousServer, previousCommon })
	t.Setenv("DTAIL_HOSTNAME_OVERRIDE", "runtime-host")
	t.Setenv("HOSTNAME", "environment-host")

	args := &Args{
		ConfigFile: "none",
		NoAuthKey:  true,
		SSHPort:    DefaultSSHPort,
	}
	cfg, err := SetupRuntime(source.Client, args, nil)
	if err != nil {
		t.Fatalf("SetupRuntime: %v", err)
	}
	if Client != sentinelClient || Server != sentinelServer || Common != sentinelCommon {
		t.Fatal("SetupRuntime mutated legacy configuration globals")
	}
	if cfg.Client == nil || cfg.Server == nil || cfg.Common == nil {
		t.Fatalf("SetupRuntime returned incomplete config: %#v", cfg)
	}
	if got, err := cfg.Hostname(); err != nil || got != "runtime-host" {
		t.Fatalf("RuntimeConfig.Hostname() = %q, %v; want runtime-host", got, err)
	}
	if args.HostnameOverride != "runtime-host" {
		t.Fatalf("args hostname = %q, want runtime-host", args.HostnameOverride)
	}
	if got := os.Getenv("DTAIL_HOSTNAME_OVERRIDE"); got != "runtime-host" {
		t.Fatalf("SetupRuntime mutated hostname environment to %q", got)
	}
	if got := os.Getenv("HOSTNAME"); got != "environment-host" {
		t.Fatalf("SetupRuntime mutated HOSTNAME to %q", got)
	}
}

func TestSetupLogDirectoryReturnsHomeLookupError(t *testing.T) {
	original := userHomeDirectory
	userHomeDirectory = func() (string, error) { return "", errors.New("lookup failed") }
	t.Cleanup(func() { userHomeDirectory = original })

	in := initializer{Common: &CommonConfig{LogDir: "~/logs"}}
	err := setupLogDirectory(&in)
	if err == nil || !strings.Contains(err.Error(), "lookup failed") {
		t.Fatalf("setupLogDirectory error = %v, want wrapped lookup failure", err)
	}
}

func TestSetupRejectsInvalidSSHPort(t *testing.T) {
	for _, port := range []int{-1, 0, 65536} {
		t.Run(fmt.Sprintf("port_%d", port), func(t *testing.T) {
			err := Setup(source.Client, &Args{ConfigFile: "none", SSHPort: port}, nil)
			if err == nil || !strings.Contains(err.Error(), "ssh port must be between 1 and 65535") {
				t.Fatalf("Setup SSHPort %d error = %v, want port range error", port, err)
			}
		})
	}
}

func TestParseConfigLoadsDefaultXDGConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	configPath := filepath.Join(home, ".config", "dtail", "dtail.conf")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	writeTestConfig(t, configPath, `{"Common":{"LogLevel":"debug"}}`)

	in := initializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}

	if err := in.parseConfig(&Args{}); err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}
	if in.Common.LogLevel != "debug" {
		t.Fatalf("expected log level debug, got %q", in.Common.LogLevel)
	}
}

// TestParseConfigFirstWins verifies that when both candidate config files
// exist, the XDG path (~/.config/dtail/dtail.conf) takes precedence and the
// second file (~/.dtail.conf) is ignored entirely — no silent merging.
func TestParseConfigFirstWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	xdgPath := filepath.Join(home, ".config", "dtail", "dtail.conf")
	if err := os.MkdirAll(filepath.Dir(xdgPath), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	writeTestConfig(t, xdgPath, `{"Common":{"LogLevel":"warn"}}`)

	homePath := filepath.Join(home, ".dtail.conf")
	// The second file would override LogLevel to "error" if merging occurred.
	writeTestConfig(t, homePath, `{"Common":{"LogLevel":"error"}}`)

	in := initializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}

	if err := in.parseConfig(&Args{}); err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}
	// First-wins: the XDG config must have set the level; the home config
	// must have been skipped, so "error" must NOT appear.
	if in.Common.LogLevel != "warn" {
		t.Fatalf("expected log level warn (first file wins), got %q", in.Common.LogLevel)
	}
}

// TestParseConfigFallsBackToHomeConfig verifies that when only the legacy
// ~/.dtail.conf exists it is loaded as the effective configuration.
func TestParseConfigFallsBackToHomeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Only create the fallback file; the XDG directory does not exist.
	homePath := filepath.Join(home, ".dtail.conf")
	writeTestConfig(t, homePath, `{"Common":{"LogLevel":"error"}}`)

	in := initializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}

	if err := in.parseConfig(&Args{}); err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}
	if in.Common.LogLevel != "error" {
		t.Fatalf("expected log level error from fallback config, got %q", in.Common.LogLevel)
	}
}

func TestParseConfigIgnoresRetiredOutputTimingKeys(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "retired-output-timing.conf")
	writeTestConfig(t, configPath, `{
  "Server": {
    "MaxLineLength": 2048,
    "OutputTransmissionDelayMs": 1,
    "OutputEOFWaitBaseMs": 2,
    "OutputEOFWaitPerFileMs": 3,
    "OutputEOFWaitMaxMs": 4,
    "OutputFlushPollIntervalMs": 5,
    "ShutdownOutputSerializeWaitMs": 6,
    "ShutdownIdleRecheckWaitMs": 7
  }
}`)

	in := initializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}
	if err := in.parseConfig(&Args{ConfigFile: configPath}); err != nil {
		t.Fatalf("parseConfig with retired output timing keys: %v", err)
	}
	if got := in.Server.MaxLineLength; got != 2048 {
		t.Fatalf("MaxLineLength = %d, want known sibling field 2048", got)
	}
}

// TestParseConfigNoConfigFile verifies that parseConfig succeeds without
// error when neither candidate config file is present.
func TestParseConfigNoConfigFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	in := initializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}

	// No config files created; must return nil, not an error.
	if err := in.parseConfig(&Args{}); err != nil {
		t.Fatalf("expected no error when no config file exists, got: %v", err)
	}
}

// TestResolveSSHKeyPath verifies the three-level precedence used when
// resolving the effective SSH private key path.
func TestResolveSSHKeyPath(t *testing.T) {
	tests := []struct {
		name     string
		cli      string
		authKey  string
		legacy   string
		expected string
	}{
		{
			name:     "cli flag wins over both env vars",
			cli:      "/cli/key",
			authKey:  "/auth/key",
			legacy:   "/legacy/key",
			expected: "/cli/key",
		},
		{
			name:     "DTAIL_AUTH_KEY_PATH wins over legacy when cli is empty",
			cli:      "",
			authKey:  "/auth/key",
			legacy:   "/legacy/key",
			expected: "/auth/key",
		},
		{
			name:     "DTAIL_SSH_PRIVATE_KEYFILE_PATH used when auth key env is also empty",
			cli:      "",
			authKey:  "",
			legacy:   "/legacy/key",
			expected: "/legacy/key",
		},
		{
			name:     "all empty returns empty string",
			cli:      "",
			authKey:  "",
			legacy:   "",
			expected: "",
		},
		{
			name:     "cli wins when only cli is set",
			cli:      "/cli/key",
			authKey:  "",
			legacy:   "",
			expected: "/cli/key",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveSSHKeyPath(tc.cli, tc.authKey, tc.legacy)
			if got != tc.expected {
				t.Fatalf("resolveSSHKeyPath(%q, %q, %q) = %q; want %q",
					tc.cli, tc.authKey, tc.legacy, got, tc.expected)
			}
		})
	}
}

func TestIntegrationSSHPrivateKeyPath(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		want       string
	}{
		{name: "Configured", configured: "/tmp/integration/id_rsa", want: "/tmp/integration/id_rsa"},
		{name: "LegacyFallback", want: "./id_rsa"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := integrationSSHPrivateKeyPath(tc.configured); got != tc.want {
				t.Fatalf("integrationSSHPrivateKeyPath(%q) = %q, want %q", tc.configured, got, tc.want)
			}
		})
	}
}

func TestIntegrationSSHPrivateKeyPathCompatibility(t *testing.T) {
	t.Run("Environment", func(t *testing.T) {
		t.Setenv("DTAIL_AUTH_KEY_PATH", "/tmp/integration/id_rsa")
		if got := IntegrationSSHPrivateKeyPath(); got != "/tmp/integration/id_rsa" {
			t.Fatalf("IntegrationSSHPrivateKeyPath() = %q, want environment path", got)
		}
	})

	t.Run("LegacyFallback", func(t *testing.T) {
		t.Setenv("DTAIL_AUTH_KEY_PATH", "")
		if got := IntegrationSSHPrivateKeyPath(); got != legacyIntegrationSSHPrivateKeyPath {
			t.Fatalf("IntegrationSSHPrivateKeyPath() = %q, want %q", got, legacyIntegrationSSHPrivateKeyPath)
		}
	})
}

// TestProcessEnvVarsAuthKeyPathTakesPrecedence is a negative/regression test
// that confirms the bug described in task k6 is fixed: when both
// DTAIL_AUTH_KEY_PATH and DTAIL_SSH_PRIVATE_KEYFILE_PATH are set,
// DTAIL_AUTH_KEY_PATH must win.
func TestProcessEnvVarsAuthKeyPathTakesPrecedence(t *testing.T) {
	t.Setenv("DTAIL_INTEGRATION_TEST_RUN_MODE", "")
	t.Setenv("DTAIL_AUTH_KEY_PATH", "/env/auth/key")
	t.Setenv("DTAIL_SSH_PRIVATE_KEYFILE_PATH", "/env/legacy/key")

	in := initializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}
	args := &Args{}
	in.processEnvVars(args)

	if args.SSHPrivateKeyFilePath != "/env/auth/key" {
		t.Fatalf("expected DTAIL_AUTH_KEY_PATH to win, got %q", args.SSHPrivateKeyFilePath)
	}
}

// TestProcessEnvVarsLegacyFallback verifies that DTAIL_SSH_PRIVATE_KEYFILE_PATH
// is still applied when DTAIL_AUTH_KEY_PATH is not set.
func TestProcessEnvVarsLegacyFallback(t *testing.T) {
	t.Setenv("DTAIL_INTEGRATION_TEST_RUN_MODE", "")
	t.Setenv("DTAIL_AUTH_KEY_PATH", "")
	t.Setenv("DTAIL_SSH_PRIVATE_KEYFILE_PATH", "/env/legacy/key")

	in := initializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}
	args := &Args{}
	in.processEnvVars(args)

	if args.SSHPrivateKeyFilePath != "/env/legacy/key" {
		t.Fatalf("expected legacy env var to be used, got %q", args.SSHPrivateKeyFilePath)
	}
}

// TestProcessEnvVarsCLIFlagNotOverridden verifies that an explicit CLI flag
// value is not overridden by either environment variable.
func TestProcessEnvVarsCLIFlagNotOverridden(t *testing.T) {
	t.Setenv("DTAIL_INTEGRATION_TEST_RUN_MODE", "")
	t.Setenv("DTAIL_AUTH_KEY_PATH", "/env/auth/key")
	t.Setenv("DTAIL_SSH_PRIVATE_KEYFILE_PATH", "/env/legacy/key")

	in := initializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}
	args := &Args{SSHPrivateKeyFilePath: "/cli/explicit/key"}
	in.processEnvVars(args)

	if args.SSHPrivateKeyFilePath != "/cli/explicit/key" {
		t.Fatalf("expected CLI flag to be preserved, got %q", args.SSHPrivateKeyFilePath)
	}
}

func TestProcessEnvVarsMapsIntegrationModeToConfig(t *testing.T) {
	t.Setenv("DTAIL_INTEGRATION_TEST_RUN_MODE", "yes")
	t.Setenv("DTAIL_AUTH_KEY_PATH", "/tmp/integration/id_rsa")
	t.Setenv("DTAIL_HOSTNAME_OVERRIDE", "")

	in := initializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}
	args := &Args{}
	in.processEnvVars(args)

	if got := in.Common.HostnameOverride; got != integrationHostname {
		t.Fatalf("HostnameOverride = %q, want %q", got, integrationHostname)
	}
	if got := in.Client.KnownHostsPath; got != integrationKnownHostsPath {
		t.Fatalf("KnownHostsPath = %q, want %q", got, integrationKnownHostsPath)
	}
	if got := in.Server.AuthorizedKeysPath; got != "/tmp/integration/id_rsa.pub" {
		t.Fatalf("AuthorizedKeysPath = %q, want integration public key", got)
	}
	if got := in.Server.HostKeyPath; got != integrationHostKeyPath {
		t.Fatalf("HostKeyPath = %q, want %q", got, integrationHostKeyPath)
	}
	if got := in.Server.MaxLineLength; got != 1024 {
		t.Fatalf("MaxLineLength = %d, want 1024", got)
	}
	if got := args.SSHPrivateKeyFallbackPaths; len(got) != 1 || got[0] != "/tmp/integration/id_rsa" {
		t.Fatalf("SSHPrivateKeyFallbackPaths = %v, want integration private key", got)
	}
}

func TestProcessEnvVarsPreservesExplicitSecurityPaths(t *testing.T) {
	t.Setenv("DTAIL_INTEGRATION_TEST_RUN_MODE", "yes")
	t.Setenv("DTAIL_AUTH_KEY_PATH", "/tmp/integration/id_rsa")
	t.Setenv("DTAIL_HOSTNAME_OVERRIDE", "/env/hostname")

	in := initializer{
		Common: &CommonConfig{HostnameOverride: "config-host"},
		Server: &ServerConfig{
			HostKeyFile:        "/config/legacy-host-key",
			HostKeyPath:        "/config/host-key",
			AuthorizedKeysPath: "/config/authorized-keys",
		},
		Client: &ClientConfig{KnownHostsPath: "/config/known-hosts"},
	}
	args := &Args{
		HostnameOverride:   "cli-host",
		KnownHostsPath:     "/cli/known-hosts",
		AuthorizedKeysPath: "/cli/authorized-keys",
		HostKeyPath:        "/cli/host-key",
	}
	in.processEnvVars(args)

	if got := in.Common.HostnameOverride; got != "config-host" {
		t.Fatalf("config HostnameOverride changed to %q", got)
	}
	if got := in.Client.KnownHostsPath; got != "/config/known-hosts" {
		t.Fatalf("config KnownHostsPath changed to %q", got)
	}
	if got := in.Server.AuthorizedKeysPath; got != "/config/authorized-keys" {
		t.Fatalf("config AuthorizedKeysPath changed to %q", got)
	}
	if got := in.Server.HostKeyPath; got != "/config/host-key" {
		t.Fatalf("config HostKeyPath changed to %q", got)
	}
}

func TestProcessEnvVarsPreservesExplicitLegacyDefaultHostKeyPath(t *testing.T) {
	t.Setenv("DTAIL_INTEGRATION_TEST_RUN_MODE", "yes")
	t.Setenv("DTAIL_AUTH_KEY_PATH", "/tmp/integration/id_rsa")

	in := initializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}
	in.Server.HostKeyFile = defaultServerHostKeyPath
	in.processEnvVars(&Args{})

	if got := in.Server.HostKeyPath; got != "" {
		t.Fatalf("integration mode set HostKeyPath to %q despite explicit legacy HostKeyFile", got)
	}
	if got := in.Server.EffectiveHostKeyPath(); got != defaultServerHostKeyPath {
		t.Fatalf("EffectiveHostKeyPath = %q, want explicit legacy path %q", got, defaultServerHostKeyPath)
	}
}

func TestDefaultSecurityPathConfig(t *testing.T) {
	common := newDefaultCommonConfig()
	client := newDefaultClientConfig()
	server := newDefaultServerConfig()

	if common.HostnameOverride != "" || client.KnownHostsPath != "" ||
		server.AuthorizedKeysPath != "" || server.HostKeyPath != "" {
		t.Fatalf("explicit path defaults should be empty: common=%+v client=%+v server=%+v",
			common, client, server)
	}
	if got := server.EffectiveHostKeyPath(); got != defaultServerHostKeyPath {
		t.Fatalf("EffectiveHostKeyPath = %q, want %q", got, defaultServerHostKeyPath)
	}
	server.HostKeyPath = "/explicit/host-key"
	if got := server.EffectiveHostKeyPath(); got != "/explicit/host-key" {
		t.Fatalf("explicit EffectiveHostKeyPath = %q", got)
	}
	server.HostKeyPath = ""
	server.HostKeyFile = "/legacy/host-key"
	if got := server.EffectiveHostKeyPath(); got != "/legacy/host-key" {
		t.Fatalf("legacy EffectiveHostKeyPath = %q", got)
	}
}

func TestSetupLoadsExplicitSecurityPaths(t *testing.T) {
	originalCommon, originalClient, originalServer := Common, Client, Server
	t.Cleanup(func() {
		Common, Client, Server = originalCommon, originalClient, originalServer
	})
	t.Setenv("DTAIL_INTEGRATION_TEST_RUN_MODE", "")
	t.Setenv("DTAIL_HOSTNAME_OVERRIDE", "")
	t.Setenv("DTAIL_AUTH_KEY_PATH", "")
	t.Setenv("DTAIL_SSH_PRIVATE_KEYFILE_PATH", "")

	configPath := filepath.Join(t.TempDir(), "security-paths.conf")
	writeTestConfig(t, configPath, `{
  "Common": {"HostnameOverride": "configured-host"},
  "Client": {"KnownHostsPath": "/config/known_hosts"},
  "Server": {
    "AuthorizedKeysPath": "/config/authorized_keys",
    "HostKeyPath": "/config/ssh_host_key"
  }
}`)

	clientArgs := &Args{ConfigFile: configPath, SSHPort: DefaultSSHPort}
	if err := Setup(source.Client, clientArgs, nil); err != nil {
		t.Fatalf("Setup client: %v", err)
	}
	if clientArgs.HostnameOverride != "configured-host" ||
		clientArgs.KnownHostsPath != "/config/known_hosts" {
		t.Fatalf("client args did not receive config paths: %+v", clientArgs)
	}
	if hostname, err := Hostname(); err != nil || hostname != "configured-host" {
		t.Fatalf("Hostname() = %q, %v; want configured-host", hostname, err)
	}

	serverArgs := &Args{ConfigFile: configPath, SSHPort: DefaultSSHPort}
	if err := Setup(source.Server, serverArgs, nil); err != nil {
		t.Fatalf("Setup server: %v", err)
	}
	if serverArgs.AuthorizedKeysPath != "/config/authorized_keys" ||
		serverArgs.HostKeyPath != "/config/ssh_host_key" {
		t.Fatalf("server args did not receive config paths: %+v", serverArgs)
	}
}

func TestSetupFlagsOverrideConfiguredSecurityPaths(t *testing.T) {
	t.Setenv("DTAIL_INTEGRATION_TEST_RUN_MODE", "")
	t.Setenv("DTAIL_HOSTNAME_OVERRIDE", "environment-host")

	in := initializer{
		Common: &CommonConfig{HostnameOverride: "configured-host", SSHPort: DefaultSSHPort},
		Server: &ServerConfig{
			AuthorizedKeysPath: "/config/authorized_keys",
			HostKeyPath:        "/config/ssh_host_key",
		},
		Client: &ClientConfig{KnownHostsPath: "/config/known_hosts", AuthKeyDisable: true},
	}
	args := &Args{
		HostnameOverride:   "flag-host",
		KnownHostsPath:     "/flag/known_hosts",
		AuthorizedKeysPath: "/flag/authorized_keys",
		HostKeyPath:        "/flag/ssh_host_key",
		SSHPort:            DefaultSSHPort,
	}
	in.processEnvVars(args)
	if err := in.setupConfig(transformServer, args, nil); err != nil {
		t.Fatalf("setupConfig: %v", err)
	}

	if in.Common.HostnameOverride != "flag-host" ||
		in.Server.AuthorizedKeysPath != "/flag/authorized_keys" ||
		in.Server.HostKeyPath != "/flag/ssh_host_key" {
		t.Fatalf("flag values did not override config: common=%+v server=%+v", in.Common, in.Server)
	}
}

// TestDefaultAuthKeyPathNoLiteralTilde is a regression test for the bug
// described in task l6: when neither os.UserHomeDir() nor the HOME environment
// variable can be resolved, defaultAuthKeyPath must return "" rather than the
// literal string "~/.ssh/id_rsa" which the SSH library cannot expand.
func TestDefaultAuthKeyPathNoLiteralTilde(t *testing.T) {
	// Unset HOME so that os.UserHomeDir() fails and the fallback env var is
	// also empty. t.Setenv restores the original value after the test.
	t.Setenv("HOME", "")

	got := defaultAuthKeyPath()
	if got == "~/.ssh/id_rsa" {
		t.Fatal("defaultAuthKeyPath returned literal '~/.ssh/id_rsa' when HOME is empty; expected \"\"")
	}
	if got != "" {
		t.Fatalf("defaultAuthKeyPath returned %q when HOME is empty; expected \"\"", got)
	}
}

// TestDefaultAuthKeyPathWithHome verifies that when HOME is set, the returned
// path is an absolute path constructed with filepath.Join (no literal '~').
func TestDefaultAuthKeyPathWithHome(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")

	got := defaultAuthKeyPath()
	want := "/home/testuser/.ssh/id_rsa"
	if got != want {
		t.Fatalf("defaultAuthKeyPath() = %q; want %q", got, want)
	}
}

func TestSetupAdditionalArgsUsesProvidedArguments(t *testing.T) {
	args := &Args{}
	setupAdditionalArgs(args, []string{"first.log", "select count($time)", "second.log"})

	if got, want := args.What, "first.log,second.log"; got != want {
		t.Fatalf("What = %q, want %q", got, want)
	}
	if got, want := args.QueryStr, "select count($time)"; got != want {
		t.Fatalf("QueryStr = %q, want %q", got, want)
	}
}

func writeTestConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config failed: %v", err)
	}
}

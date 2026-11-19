package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/source"
)

// Used to initialize the configuration.
type initializer struct {
	Common *CommonConfig
	Server *ServerConfig
	Client *ClientConfig
}

const (
	legacyIntegrationSSHPrivateKeyPath = "./id_rsa"
	integrationHostname                = "integrationtest"
	integrationKnownHostsPath          = "./known_hosts"
	integrationHostKeyPath             = "./ssh_host_key"
)

type transformCb func(*initializer, *Args, []string) error

var userHomeDirectory = os.UserHomeDir

// configWarningOutput receives non-fatal config file warnings. Tests replace it.
var configWarningOutput io.Writer = os.Stderr

func (in *initializer) parseConfig(args *Args) error {
	if strings.ToLower(args.ConfigFile) == "none" {
		return nil
	}

	if args.ConfigFile != "" {
		return in.parseSpecificConfig(args.ConfigFile)
	}

	homeDir, err := userHomeDirectory()
	if err == nil && homeDir != "" {
		// Search candidate paths in priority order. The first existing file
		// wins: ~/.config/dtail/dtail.conf takes precedence over ~/.dtail.conf.
		// Loading both would silently merge scalar fields (later-file wins),
		// which is surprising and hard to debug.
		paths := []string{
			fmt.Sprintf("%s/.config/dtail/dtail.conf", homeDir),
			fmt.Sprintf("%s/.dtail.conf", homeDir),
		}
		for _, configPath := range paths {
			if _, err := os.Stat(configPath); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return err
			}
			// Stop after loading the first file that exists.
			return in.parseSpecificConfig(configPath)
		}
	}

	return nil
}

func (in *initializer) parseSpecificConfig(configFile string) error {
	fd, err := os.Open(configFile)
	if err != nil {
		return fmt.Errorf("unable to read config file: %w", err)
	}
	defer func() { _ = fd.Close() }()

	cfgBytes, err := io.ReadAll(fd)
	if err != nil {
		return fmt.Errorf("unable to read config file %s: %w", configFile, err)
	}

	color.TakeConfigWarnings() // Drop anything left over from an earlier decode.
	if err := json.Unmarshal([]byte(cfgBytes), in); err != nil {
		color.TakeConfigWarnings()
		return fmt.Errorf("unable to parse config file %s: %w", configFile, err)
	}
	// An unrecognised colour value falls back to its default instead of failing,
	// so an old config file keeps loading (dserver included). The logger is not
	// set up yet, so the warnings go to stderr, which keeps stdout payload clean.
	for _, warning := range color.TakeConfigWarnings() {
		_, _ = fmt.Fprintf(configWarningOutput, "WARN: config file %s: %s\n", configFile, warning)
	}

	return nil
}

func (in *initializer) transformConfig(sourceProcess source.Source, args *Args,
	additionalArgs []string) error {

	in.processEnvVars(args)
	in.applyLoggingDefaults(sourceProcess)

	switch sourceProcess {
	case source.Server:
		return in.setupConfig(transformServer, args, additionalArgs)
	case source.Client:
		return in.setupConfig(transformClient, args, additionalArgs)
	case source.HealthCheck:
		return in.setupConfig(transformHealthCheck, args, additionalArgs)
	default:
		return fmt.Errorf("unable to transform config, unknown source '%s'",
			sourceProcess)
	}
}

// applyLoggingDefaults fills the logger and log directory the config file left
// unset with the per-command defaults. The --logger and --logDir flags default
// to empty, so setupConfig applies them afterwards only when given, which
// yields the precedence: explicit flag > config file > per-command default.
//
// dtailhealth is the exception: it has no --cfg flag and reads the user's
// client config, so a Common.Logger or Common.LogDir meant for the interactive
// clients would change its monitoring-check output and create log directories.
// It therefore ignores both config values, like it ignores Common.LogLevel,
// and only an explicit --logger flag changes its logger.
func (in *initializer) applyLoggingDefaults(sourceProcess source.Source) {
	logger, logDir := loggingDefaults(sourceProcess)
	if sourceProcess == source.HealthCheck {
		in.Common.Logger = logger
		in.Common.LogDir = logDir
		return
	}
	if in.Common.Logger == "" {
		in.Common.Logger = logger
	}
	if in.Common.LogDir == "" {
		in.Common.LogDir = logDir
	}
}

// loggingDefaults returns the default logger and log directory of a command.
func loggingDefaults(sourceProcess source.Source) (logger, logDir string) {
	switch sourceProcess {
	case source.Server:
		return DefaultServerLogger, DefaultServerLogDir
	case source.HealthCheck:
		return DefaultHealthCheckLogger, DefaultServerLogDir
	default:
		return DefaultClientLogger, DefaultClientLogDir
	}
}

func (in *initializer) processEnvVars(args *Args) {
	// Resolve SSH private key path from environment variables.
	// DTAIL_AUTH_KEY_PATH is the documented alias and takes precedence.
	// DTAIL_SSH_PRIVATE_KEYFILE_PATH is the legacy name and is only used when
	// DTAIL_AUTH_KEY_PATH is not set, so that the documented env var always wins.
	// Neither env var overrides an explicitly supplied CLI flag value.
	args.SSHPrivateKeyFilePath = resolveSSHKeyPath(
		args.SSHPrivateKeyFilePath,
		os.Getenv("DTAIL_AUTH_KEY_PATH"),
		os.Getenv("DTAIL_SSH_PRIVATE_KEYFILE_PATH"),
	)
	if args.HostnameOverride == "" {
		args.HostnameOverride = os.Getenv("DTAIL_HOSTNAME_OVERRIDE")
	}

	if Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		in.applyIntegrationDefaults(args, integrationSSHPrivateKeyPath(os.Getenv("DTAIL_AUTH_KEY_PATH")))
	}

	// Note: the direct-output read/aggregate path is now the one and only runtime
	// path. The historical disable toggle (a former env var and its matching
	// server config field) no longer exists and is not read here. Old configs
	// that still set that JSON key, or callers that still export the old env var,
	// keep working: unknown JSON keys are silently ignored by the lenient decoder
	// and an unread env var has no effect.
}

// applyIntegrationDefaults translates the legacy integration-test switch into
// ordinary configuration once. Explicit CLI and config-file values win. The
// additional bootstrap key preserves the harness's ability to test auth-key
// registration with a key other than the suite's initially authorized key.
func (in *initializer) applyIntegrationDefaults(args *Args, bootstrapKeyPath string) {
	if args.HostnameOverride == "" && in.Common.HostnameOverride == "" {
		in.Common.HostnameOverride = integrationHostname
	}
	if args.KnownHostsPath == "" && in.Client.KnownHostsPath == "" {
		in.Client.KnownHostsPath = integrationKnownHostsPath
	}
	if args.AuthorizedKeysPath == "" && in.Server.AuthorizedKeysPath == "" {
		in.Server.AuthorizedKeysPath = bootstrapKeyPath + ".pub"
	}
	if args.HostKeyPath == "" && in.Server.HostKeyPath == "" && in.Server.HostKeyFile == "" {
		in.Server.HostKeyPath = integrationHostKeyPath
	}
	if !containsString(args.SSHPrivateKeyFallbackPaths, bootstrapKeyPath) {
		args.SSHPrivateKeyFallbackPaths = append(args.SSHPrivateKeyFallbackPaths, bootstrapKeyPath)
	}
	in.Server.MaxLineLength = 1024
}

func integrationSSHPrivateKeyPath(configuredPath string) string {
	if configuredPath != "" {
		return configuredPath
	}
	return legacyIntegrationSSHPrivateKeyPath
}

// resolveSSHKeyPath returns the effective SSH private key file path, applying
// the following precedence (highest to lowest):
//  1. cliValue  — an explicit flag value supplied by the user
//  2. authKeyEnv — DTAIL_AUTH_KEY_PATH (the documented alias)
//  3. legacyEnv  — DTAIL_SSH_PRIVATE_KEYFILE_PATH (the legacy name)
func resolveSSHKeyPath(cliValue, authKeyEnv, legacyEnv string) string {
	if cliValue != "" {
		return cliValue
	}
	if authKeyEnv != "" {
		return authKeyEnv
	}
	return legacyEnv
}

func (in *initializer) setupConfig(sourceCb transformCb, args *Args,
	additionalArgs []string) error {

	// Copy args to config objects.
	// NEXT: Maybe unify args and config structs?
	if args.SSHPort != DefaultSSHPort {
		in.Common.SSHPort = args.SSHPort
	}
	applyStringOverride(&args.HostnameOverride, &in.Common.HostnameOverride)
	if args.LogLevel != DefaultLogLevel {
		in.Common.LogLevel = args.LogLevel
	}
	if args.NoColor {
		in.Client.TermColorsEnable = false
	}
	if args.NoAuthKey {
		in.Client.AuthKeyDisable = true
	}
	if in.Client.AuthKeyDisable {
		args.NoAuthKey = true
	}
	if args.SSHPrivateKeyFilePath == "" {
		args.SSHPrivateKeyFilePath = in.Client.AuthKeyPath
	}
	if args.SSHPrivateKeyFilePath != "" {
		in.Client.AuthKeyPath = args.SSHPrivateKeyFilePath
	}
	// Warn early when the auth-key path cannot be determined and auth-key is
	// still enabled. The SSH stack does not expand '~', so a literal path
	// would silently fail later; warning here points the operator at the fix.
	if !in.Client.AuthKeyDisable && in.Client.AuthKeyPath == "" {
		fmt.Fprintf(os.Stderr,
			"WARN: cannot determine home directory; auth-key fast reconnect disabled. "+
				"Set DTAIL_AUTH_KEY_PATH explicitly to re-enable it.\n")
		in.Client.AuthKeyDisable = true
		args.NoAuthKey = true
	}
	// The logging flags default to empty: a non-empty value was given
	// explicitly and wins over the config file and the per-command default.
	if args.LogDir != "" {
		in.Common.LogDir = args.LogDir
	}
	if args.Logger != "" {
		in.Common.Logger = args.Logger
	}
	// Opt in to teeing retrieved payload into the client log file. The flag can
	// only turn it on; a config-file value (Client.LogPayload) is preserved when
	// the flag is not given, since the flag defaults to false.
	if args.LogPayload {
		in.Client.LogPayload = true
	}
	if args.ConnectionsPerCPU == 0 {
		args.ConnectionsPerCPU = DefaultConnectionsPerCPU
	}
	if in.Common.SSHPort < 1 || in.Common.SSHPort > 65535 {
		return fmt.Errorf("ssh port must be between 1 and 65535, got %d", in.Common.SSHPort)
	}

	if err := setupLogDirectory(in); err != nil {
		return err
	}
	if err := sourceCb(in, args, additionalArgs); err != nil {
		return err
	}
	if args.Plain {
		setupPlainMode(in, args)
	}
	if args.What == "" {
		setupAdditionalArgs(args, additionalArgs)
	}

	return nil
}

func setupLogDirectory(in *initializer) error {
	// Setup log directory.
	if strings.Contains(in.Common.LogDir, "~/") {
		homeDir, err := userHomeDirectory()
		if err != nil {
			return fmt.Errorf("resolve home directory for log path %q: %w", in.Common.LogDir, err)
		}
		in.Common.LogDir = strings.ReplaceAll(in.Common.LogDir, "~/",
			fmt.Sprintf("%s/", homeDir))
	}
	return nil
}

func setupPlainMode(in *initializer, args *Args) {
	args.Quiet = true
	args.NoColor = true
	in.Client.TermColorsEnable = false
	if args.LogLevel == "" {
		args.LogLevel = "ERROR"
		in.Common.LogLevel = "ERROR"
	}
}

func setupAdditionalArgs(args *Args, additionalArgs []string) {
	// Interpret additional args as file list or as query.
	if args.What == "" {
		var files []string
		for _, arg := range additionalArgs {
			if args.QueryStr == "" && strings.Contains(strings.ToLower(arg), "select ") {
				args.QueryStr = arg
				continue
			}
			files = append(files, arg)
		}
		args.What = strings.Join(files, ",")
	}
}

func transformClient(in *initializer, args *Args, additionalArgs []string) error {
	applyStringOverride(&args.KnownHostsPath, &in.Client.KnownHostsPath)
	// Serverless mode.
	if args.Discovery == "" && (args.ServersStr == "" ||
		strings.ToLower(args.ServersStr) == "serverless") {
		// We are not connecting to any servers.
		args.Serverless = true
		if args.LogLevel == DefaultLogLevel {
			in.Common.LogLevel = "warn"
		}
	}
	return nil
}

func transformServer(in *initializer, args *Args, additionalArgs []string) error {
	applyStringOverride(&args.AuthorizedKeysPath, &in.Server.AuthorizedKeysPath)
	applyStringOverride(&args.HostKeyPath, &in.Server.HostKeyPath)
	if args.SSHBindAddress != "" {
		in.Server.SSHBindAddress = args.SSHBindAddress
	}
	return nil
}

func transformHealthCheck(in *initializer, args *Args, additionalArgs []string) error {
	// Serverless mode.
	if args.Discovery == "" && (args.ServersStr == "" ||
		strings.ToLower(args.ServersStr) == "serverless") {
		// We are not connecting to any servers.
		args.Serverless = true
		in.Common.LogLevel = "warn"
	}
	args.TrustAllHosts = true
	return nil
}

func applyStringOverride(argumentValue, configValue *string) {
	if *argumentValue == "" {
		*argumentValue = *configValue
		return
	}
	*configValue = *argumentValue
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/mimecast/dtail/internal/source"
)

// Used to initialize the configuration.
type initializer struct {
	Common *CommonConfig
	Server *ServerConfig
	Client *ClientConfig
}

type transformCb func(*initializer, *Args, []string) error

func (in *initializer) parseConfig(args *Args) error {
	if strings.ToLower(args.ConfigFile) == "none" {
		return nil
	}

	if args.ConfigFile != "" {
		return in.parseSpecificConfig(args.ConfigFile)
	}

	if homeDir, err := os.UserHomeDir(); err != nil {
		var paths []string
		paths = append(paths, fmt.Sprintf("%s/.config/dtail/dtail.conf", homeDir))
		paths = append(paths, fmt.Sprintf("%s/.dtail.conf", homeDir))
		for _, configPath := range paths {
			if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				in.parseSpecificConfig(configPath)
			}
		}
	}

	return nil
}

func (in *initializer) parseSpecificConfig(configFile string) error {
	fd, err := os.Open(configFile)
	if err != nil {
		return fmt.Errorf("Unable to read config file: %v", err)
	}
	defer fd.Close()

	cfgBytes, err := ioutil.ReadAll(fd)
	if err != nil {
		return fmt.Errorf("Unable to read config file %s: %v", configFile, err)
	}

	if err := json.Unmarshal([]byte(cfgBytes), in); err != nil {
		return fmt.Errorf("Unable to parse config file %s: %v", configFile, err)
	}

	return nil
}

func (in *initializer) transformConfig(sourceProcess source.Source, args *Args,
	additionalArgs []string) error {

	in.readEnvironmentVars()

	switch sourceProcess {
	case source.Server:
		return in.optimusPrime(transformServer, args, additionalArgs)
	case source.Client:
		return in.optimusPrime(transformClient, args, additionalArgs)
	case source.HealthCheck:
		return in.optimusPrime(transformHealthCheck, args, additionalArgs)
	default:
		return fmt.Errorf("Unable to transform config, unknown source '%s'",
			sourceProcess)
	}
}

// There are some special options which can be set by environment variable.
func (in *initializer) readEnvironmentVars() {
	if Env("DTAIL_RUN_INTEGRATION_TESTS") {
		os.Setenv("DTAIL_HOSTNAME_OVERRIDE", "integrationtest")
		os.Setenv("DTAIL_SSH_KNOWN_HOSTS_FILE", "./known_hosts")
	}
}

func (in *initializer) optimusPrime(sourceCb transformCb, args *Args,
	additionalArgs []string) error {

	// Copy args to config objects.
	// NEXT: Maybe unify args and config structs?
	if args.SSHPort != DefaultSSHPort {
		in.Common.SSHPort = args.SSHPort
	}
	if args.LogLevel != DefaultLogLevel {
		in.Common.LogLevel = args.LogLevel
	}
	if args.NoColor {
		in.Client.TermColorsEnable = false
	}
	if args.LogDir != "" {
		in.Common.LogDir = args.LogDir
	}
	if args.Logger != "" {
		in.Common.Logger = args.Logger
	}
	if args.ConnectionsPerCPU == 0 {
		args.ConnectionsPerCPU = DefaultConnectionsPerCPU
	}

	// Setup log directory.
	if strings.Contains(in.Common.LogDir, "~/") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			panic(err)
		}
		in.Common.LogDir = strings.ReplaceAll(in.Common.LogDir, "~/",
			fmt.Sprintf("%s/", homeDir))
	}

	// Source type specific transormations.
	sourceCb(in, args, additionalArgs)

	// Spartan mode.
	if args.Spartan {
		args.Quiet = true
		args.NoColor = true
		in.Client.TermColorsEnable = false
		if args.LogLevel == "" {
			args.LogLevel = "ERROR"
			in.Common.LogLevel = "ERROR"
		}
	}
	// Interpret additional args as file list or as query.
	if args.What == "" {
		var files []string
		for _, arg := range flag.Args() {
			if args.QueryStr == "" && strings.Contains(strings.ToLower(arg), "select ") {
				args.QueryStr = arg
				continue
			}
			files = append(files, arg)
		}
		args.What = strings.Join(files, ",")
	}

	return nil
}

func transformClient(in *initializer, args *Args, additionalArgs []string) error {
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

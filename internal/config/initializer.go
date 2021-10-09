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

func (c *initializer) parseConfig(args *Args) error {
	if strings.ToUpper(args.ConfigFile) == "NONE" {
		return nil
	}

	if args.ConfigFile != "" {
		return c.parseSpecificConfig(args.ConfigFile)
	}

	if homeDir, err := os.UserHomeDir(); err != nil {
		var paths []string
		paths = append(paths, fmt.Sprintf("%s/.config/dtail/dtail.conf", homeDir))
		paths = append(paths, fmt.Sprintf("%s/.dtail.conf", homeDir))
		for _, configPath := range paths {
			if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				c.parseSpecificConfig(configPath)
			}
		}
	}

	return nil
}

func (c *initializer) parseSpecificConfig(configFile string) error {
	fd, err := os.Open(configFile)
	if err != nil {
		return fmt.Errorf("Unable to read config file: %v", err)
	}
	defer fd.Close()

	cfgBytes, err := ioutil.ReadAll(fd)
	if err != nil {
		return fmt.Errorf("Unable to read config file %s: %v", configFile, err)
	}

	if err := json.Unmarshal([]byte(cfgBytes), c); err != nil {
		return fmt.Errorf("Unable to parse config file %s: %v", configFile, err)
	}

	return nil
}

func (i *initializer) transformConfig(sourceProcess source.Source, args *Args, additionalArgs []string) error {

	switch sourceProcess {
	case source.Server:
		return i.optimusPrime(transformServer, args, additionalArgs)
	case source.Client:
		return i.optimusPrime(transformClient, args, additionalArgs)
	case source.HealthCheck:
		return i.optimusPrime(transformHealthCheck, args, additionalArgs)
	default:
		return fmt.Errorf("Unable to transform config, unknown source '%s'", sourceProcess)
	}
}

func (i *initializer) optimusPrime(sourceCb transformCb, args *Args, additionalArgs []string) error {
	// Copy args to config objects.
	if args.SSHPort != DefaultSSHPort {
		i.Common.SSHPort = args.SSHPort
	}
	if args.LogLevel != DefaultLogLevel {
		i.Common.LogLevel = args.LogLevel
	}
	if args.NoColor {
		i.Client.TermColorsEnable = false
	}
	if args.LogDir != "" {
		i.Common.LogDir = args.LogDir
	}
	if args.Logger != "" {
		i.Common.Logger = args.Logger
	}
	if args.ConnectionsPerCPU == 0 {
		args.ConnectionsPerCPU = DefaultConnectionsPerCPU
	}

	// Setup log directory.
	if strings.Contains(i.Common.LogDir, "~/") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			panic(err)
		}
		i.Common.LogDir = strings.ReplaceAll(i.Common.LogDir, "~/", fmt.Sprintf("%s/", homeDir))
	}

	// Source type specific transormations.
	sourceCb(i, args, additionalArgs)

	// Spartan mode.
	if args.Spartan {
		args.Quiet = true
		args.NoColor = true
		i.Client.TermColorsEnable = false
		if args.LogLevel == "" {
			args.LogLevel = "ERROR"
			i.Common.LogLevel = "ERROR"
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

func transformClient(i *initializer, args *Args, additionalArgs []string) error {
	// Serverless mode.
	if args.Discovery == "" && (args.ServersStr == "" ||
		strings.ToLower(args.ServersStr) == "serverless") {
		// We are not connecting to any servers.
		args.Serverless = true
		i.Common.LogLevel = "warn"
	}

	return nil
}

func transformServer(i *initializer, args *Args, additionalArgs []string) error {
	return nil
}

func transformHealthCheck(i *initializer, args *Args, additionalArgs []string) error {
	// Serverless mode.
	if args.Discovery == "" && (args.ServersStr == "" ||
		strings.ToLower(args.ServersStr) == "serverless") {
		// We are not connecting to any servers.
		args.Serverless = true
		i.Common.LogLevel = "warn"
	}
	args.TrustAllHosts = true
	return nil
}

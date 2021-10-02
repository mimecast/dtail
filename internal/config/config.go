package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
)

const (
	// ControlUser is used for various DTail specific operations.
	ControlUser string = "DTAIL-CONTROL"
	// ScheduleUser is used for non-interactive scheduled mapreduce queries.
	ScheduleUser string = "DTAIL-SCHEDULE"
	// ContinuousUser is used for non-interactive continuous mapreduce queries.
	ContinuousUser string = "DTAIL-CONTINUOUS"
	// InterruptTimeoutS is used to terminate DTail when Ctrl+C was pressed twice within a given interval.
	InterruptTimeoutS int = 3
	// ConnectionsPerCPU controls how many connections are established concurrently as a start (slow start)
	DefaultConnectionsPerCPU int = 10
	// DTailSSHServerDefaultPort is the default DServer port.
	DefaultSSHPort int = 2222
)

// Client holds a DTail client configuration.
var Client *ClientConfig

// Server holds a DTail server configuration.
var Server *ServerConfig

// Common holds common configs of both both, client and server.
var Common *CommonConfig

// Used to initialize the configuration.
type configInitializer struct {
	Common *CommonConfig
	Server *ServerConfig
	Client *ClientConfig
}

func (c *configInitializer) parseConfig(args *Args) {
	if strings.ToUpper(args.ConfigFile) == "NONE" {
		return
	}

	if args.ConfigFile != "" {
		c.parseSpecificConfig(args.ConfigFile)
		return
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
}

func (c *configInitializer) parseSpecificConfig(configFile string) {
	fd, err := os.Open(configFile)
	if err != nil {
		panic(fmt.Sprintf("Unable to read config file: %v", err))
	}
	defer fd.Close()

	cfgBytes, err := ioutil.ReadAll(fd)
	if err != nil {
		panic(fmt.Sprintf("Unable to read config file %s: %v", configFile, err))
	}

	err = json.Unmarshal([]byte(cfgBytes), c)
	if err != nil {
		panic(fmt.Sprintf("Unable to parse config file %s: %v", configFile, err))
	}
}

func (c *configInitializer) transformConfig(args *Args, additionalArgs []string,
	client *ClientConfig, server *ServerConfig, common *CommonConfig) (*ClientConfig, *ServerConfig, *CommonConfig) {
	if args.LogDir != "" {
		common.LogDir = args.LogDir
		if common.LogStrategy == "" {
			common.LogStrategy = "daily"
		}
	}

	if args.LogLevel != "" {
		common.LogLevel = args.LogLevel
	}

	if args.SSHPort != DefaultSSHPort {
		common.SSHPort = args.SSHPort
	}
	if args.NoColor {
		client.TermColorsEnable = false
	}

	if args.Spartan {
		args.Quiet = true
		args.NoColor = true
	}

	if args.Discovery == "" && args.ServersStr == "" {
		args.Serverless = true
	}

	// Interpret additional args as file list.
	if args.What == "" {
		var files []string
		for _, file := range flag.Args() {
			files = append(files, file)
		}
		args.What = strings.Join(files, ",")
	}

	return client, server, common
}

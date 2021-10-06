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

func (c *initializer) parseConfig(args *Args) {
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

func (c *initializer) parseSpecificConfig(configFile string) {
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

func (c *initializer) transformConfig(sourceProcess source.Source, args *Args, additionalArgs []string,
	client *ClientConfig, server *ServerConfig, common *CommonConfig) (*ClientConfig, *ServerConfig, *CommonConfig) {
	if args.LogDir != "" {
		common.LogDir = args.LogDir
	}
	if strings.Contains(common.LogDir, "~/") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			panic(err)
		}
		common.LogDir = strings.ReplaceAll(common.LogDir, "~/", fmt.Sprintf("%s/", homeDir))
	}
	if common.LogStrategy == "" {
		common.LogStrategy = "daily"
	}

	if args.Spartan {
		args.Quiet = true
		args.NoColor = true
		if args.LogLevel == "" {
			args.LogLevel = "ERROR"
		}
	}
	if args.NoColor {
		client.TermColorsEnable = false
	}

	if args.LogLevel != "" {
		common.LogLevel = args.LogLevel
	} else if sourceProcess == source.Client && args.ServersStr == "" && args.Discovery == "" {
		// We are in serverless mode. Default log level is WARN.
		common.LogLevel = "WARN"
	}

	if args.SSHPort != DefaultSSHPort {
		common.SSHPort = args.SSHPort
	}

	if args.Discovery == "" && (args.ServersStr == "" ||
		strings.ToLower(args.ServersStr) == "serverless") {
		// We are not connecting to any servers.
		args.Serverless = true
	}

	if sourceProcess == source.HealthCheck {
		args.TrustAllHosts = true
		if !args.Serverless && strings.ToLower(args.ServersStr) == "" {
			args.ServersStr = fmt.Sprintf("localhost:%d", DefaultSSHPort)
		}
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

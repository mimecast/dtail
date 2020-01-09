package config

import (
	"encoding/json"
	"io/ioutil"
	"os"
)

// ControlUser is used for various DTail specific operations.
const ControlUser string = "DTAIL-CONTROL-USER"

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

// Parse and read a given config file in JSON format.
func (c *configInitializer) parseConfig(configFile string) {
	fd, err := os.Open(configFile)
	if err != nil {
		panic(err)
	}
	defer fd.Close()

	cfgBytes, err := ioutil.ReadAll(fd)
	if err != nil {
		panic(err)
	}

	err = json.Unmarshal([]byte(cfgBytes), c)
	if err != nil {
		panic(err)
	}
}

// Init the DTail configuration.
func Init(configFile string) {
	initializer := configInitializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}

	if configFile == "" {
		configFile = "./cfg/dtail.json"
	}

	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		initializer.parseConfig(configFile)
	}

	// Assign pointers to global variables, so that we can access the
	// configuration from any place of the program.
	Common = initializer.Common
	Server = initializer.Server
	Client = initializer.Client

	if Server.MapreduceLogFormat == "" {
		Server.MapreduceLogFormat = "default"
	}
}

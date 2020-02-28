package config

import (
	"encoding/json"
	"io/ioutil"
	"os"
)

// ControlUser is used for various DTail specific operations.
const ControlUser string = "DTAIL-CONTROL-USER"

// ScheduledUser is used for scheduled queries.
const ScheduleUser string = "DTAIL-SCHEDULED-USER"

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

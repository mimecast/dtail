package config

import (
	"os"
)

// Read the DTail configuration.
func Read(configFile string, sshPort int, noColor bool) {
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

	// If non-standard port specified, overwrite config
	if sshPort != 2222 {
		Common.SSHPort = sshPort
	}
	if noColor {
		Client.TermColorsEnable = false
	}
}

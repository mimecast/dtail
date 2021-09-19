package config

import (
	"os"
)

const NoConfigFile string = "Don't read a config file - use defaults only"

// Setup the DTail configuration.
func Setup(args *Args, additionalArgs []string) {
	initializer := configInitializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}

	if args.ConfigFile == "" {
		// TODO: Search more paths for config file (e.g. in /etc and in ~/.config/...
		args.ConfigFile = "./cfg/dtail.json"
	}

	if args.ConfigFile != NoConfigFile {
		if _, err := os.Stat(args.ConfigFile); !os.IsNotExist(err) {
			initializer.parseConfig(args.ConfigFile)
		}
	}

	// Assign pointers to global variables, so that we can access the
	// configuration from any place of the program.
	Common = initializer.Common
	Server = initializer.Server
	Client = initializer.Client

	args.transform(additionalArgs)
}

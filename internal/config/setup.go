package config

// Setup the DTail configuration.
func Setup(args *Args, additionalArgs []string) {
	initializer := configInitializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}
	initializer.parseConfig(args)
	Client, Server, Common = args.transformConfig(
		additionalArgs,
		initializer.Client,
		initializer.Server,
		initializer.Common,
	)
}

package config

import "github.com/mimecast/dtail/internal/source"

const (
	// HealthUser is used for the health check
	HealthUser string = "DTAIL-HEALTH"
	// ScheduleUser is used for non-interactive scheduled mapreduce queries.
	ScheduleUser string = "DTAIL-SCHEDULE"
	// ContinuousUser is used for non-interactive continuous mapreduce queries.
	ContinuousUser string = "DTAIL-CONTINUOUS"
	// InterruptTimeoutS specifies the Ctrl+C log pause interval.
	InterruptTimeoutS int = 3
	// DefaultConnectionsPerCPU controls how many connections are established concurrently.
	DefaultConnectionsPerCPU int = 10
	// DefaultSSHPort is the default DServer port.
	DefaultSSHPort int = 2222
	// DefaultLogLevel specifies the default log level (obviously)
	DefaultLogLevel string = "info"
	// DefaultClientLogger specifies the default logger for the client commands.
	DefaultClientLogger string = "fout"
	// DefaultServerLogger specifies the default logger for dtail server.
	DefaultServerLogger string = "file"
	// DefaultHealthCheckLogger specifies the default logger used for health checks.
	DefaultHealthCheckLogger string = "none"
)

// Client holds a DTail client configuration.
var Client *ClientConfig

// Server holds a DTail server configuration.
var Server *ServerConfig

// Common holds common configs of both both, client and server.
var Common *CommonConfig

// Setup the DTail configuration.
func Setup(sourceProcess source.Source, args *Args, additionalArgs []string) {
	initializer := initializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}
	if err := initializer.parseConfig(args); err != nil {
		panic(err)
	}
	if err := initializer.transformConfig(sourceProcess, args, additionalArgs); err != nil {
		panic(err)
	}

	// Make config accessible globally
	Server = initializer.Server
	Client = initializer.Client
	Common = initializer.Common
}

package config

import (
	"fmt"

	"github.com/mimecast/dtail/internal/source"
)

const (
	// DefaultIdleSessionTimeoutS is the rolling inactivity timeout applied to an
	// authenticated SSH connection. Successful network reads and writes refresh it.
	DefaultIdleSessionTimeoutS int = 15 * 60

	// DefaultOutputBufferMaxBytes bounds payload bytes waiting to be written to a
	// single SSH session.
	DefaultOutputBufferMaxBytes int = 2 << 20 // 2 MiB

	// DefaultMaxCommandFrameSize is the default maximum number of bytes that
	// may be buffered between two ';' delimiters in the command protocol.
	// Frames exceeding this limit cause the session to be closed immediately to
	// prevent memory exhaustion. Individual server deployments may override this
	// via ServerConfig.MaxCommandFrameSize.
	DefaultMaxCommandFrameSize int = 1 << 20 // 1 MiB

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

// Client holds DTail client configuration.
var Client *ClientConfig

// Server holds DTail server configuration.
var Server *ServerConfig

// Common holds configuration common to both client and server.
var Common *CommonConfig

// IsPasswordOnlyUser reports whether userName identifies an internal account
// whose authorization depends on a password and server-side policy checks.
func IsPasswordOnlyUser(userName string) bool {
	switch userName {
	case HealthUser, ScheduleUser, ContinuousUser:
		return true
	default:
		return false
	}
}

// SetupRuntime loads and prepares one process configuration without publishing
// it through the legacy package globals.
func SetupRuntime(sourceProcess source.Source, args *Args, additionalArgs []string) (RuntimeConfig, error) {
	initializer := initializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}
	if err := initializer.parseConfig(args); err != nil {
		return RuntimeConfig{}, fmt.Errorf("load configuration: %w", err)
	}
	if err := initializer.transformConfig(sourceProcess, args, additionalArgs); err != nil {
		return RuntimeConfig{}, fmt.Errorf("prepare configuration: %w", err)
	}

	return RuntimeConfig{
		Client: initializer.Client,
		Server: initializer.Server,
		Common: initializer.Common,
	}, nil
}

// Setup prepares the DTail configuration and publishes it through the legacy
// package globals.
//
// Deprecated: process entry points should use SetupRuntime and inject the
// returned RuntimeConfig.
func Setup(sourceProcess source.Source, args *Args, additionalArgs []string) error {
	runtimeCfg, err := SetupRuntime(sourceProcess, args, additionalArgs)
	if err != nil {
		return err
	}

	// Make config accessible globally
	Server = runtimeCfg.Server
	Client = runtimeCfg.Client
	Common = runtimeCfg.Common
	return nil
}

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/cli"
	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/io/signal"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/source"
	"github.com/mimecast/dtail/internal/version"
)

type healthClientFactory func(config.Args, config.RuntimeConfig,
	clients.LoggerDependencies) (clients.Client, error)

type pprofShutdowner interface {
	Shutdown(context.Context) error
}

type profileServer interface {
	pprofShutdowner
	Address() string
	Start(*sync.WaitGroup)
}

type healthLifecycleDependencies struct {
	stderr          io.Writer
	loggers         clients.LoggerDependencies
	newPProfServer  func(context.Context, string) (profileServer, error)
	newHealthClient healthClientFactory
}

// The evil begins here.
func main() {
	os.Exit(run())
}

func run() int {
	args := config.Args{SSHPort: config.DefaultSSHPort}
	var displayVersion bool
	var pprof string

	flag.BoolVar(&displayVersion, "version", false, "Display version")
	flag.StringVar(&args.HostnameOverride, "hostname-override", "", "Override the hostname used in logs and output")
	flag.StringVar(&args.Logger, "logger", config.DefaultHealthCheckLogger, "Logger name")
	flag.StringVar(&args.LogLevel, "logLevel", "none", "Log level")
	flag.StringVar(&args.ServersStr, "server", "", "Remote server to connect")
	flag.BoolVar(&args.NoAuthKey, "no-auth-key", false, "Disable auth-key fast reconnect feature")
	flag.StringVar(&pprof, "pprof", "", "Start PProf server this address")
	flag.Parse()

	if displayVersion {
		version.PrintAndExit(false)
	}

	runtimeCfg, err := config.SetupRuntime(source.HealthCheck, &args, flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "CRITICAL: unable to configure dtailhealth: %v\n", err)
		return 2
	}
	colorizer := brush.New(runtimeCfg.Client.TermColors)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	if err := dlog.Start(ctx, &wg, source.HealthCheck, runtimeCfg, colorizer); err != nil {
		wg.Done()
		cancel()
		fmt.Fprintf(os.Stderr, "CRITICAL: unable to initialize dtailhealth logger: %v\n", err)
		return 2
	}

	loggers := clients.NewLoggerDependencies(dlog.Client, dlog.Server, dlog.Common)
	return runHealthLifecycle(ctx, cancel, &wg, args, runtimeCfg, pprof, healthLifecycleDependencies{
		stderr:  os.Stderr,
		loggers: loggers,
		newPProfServer: func(ctx context.Context, address string) (profileServer, error) {
			return cli.NewPProfServer(ctx, address)
		},
		newHealthClient: func(args config.Args, cfg config.RuntimeConfig,
			loggers clients.LoggerDependencies) (clients.Client, error) {
			return clients.NewHealthClient(args, cfg, loggers, colorizer)
		},
	})
}

func runHealthLifecycle(ctx context.Context, cancel context.CancelFunc, wg *sync.WaitGroup,
	args config.Args, cfg config.RuntimeConfig, pprofAddress string,
	deps healthLifecycleDependencies) int {
	// Register logger cleanup first so pprof shutdown can still report errors.
	defer func() {
		cancel()
		wg.Wait()
	}()

	if pprofAddress != "" {
		pprofServer, pprofErr := deps.newPProfServer(ctx, pprofAddress)
		if pprofErr != nil {
			deps.loggers.Client.Error("Unable to start PProf", pprofErr)
		} else {
			deps.loggers.Client.Info("Starting PProf", pprofServer.Address())
			pprofServer.Start(nil)
			defer shutdownPProf(ctx, pprofServer, deps.loggers.Client)
		}
	}

	healthClient, err := deps.newHealthClient(args, cfg, deps.loggers)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "CRITICAL: unable to create dtailhealth client: %v\n", err)
		return 2
	}
	return healthClient.Start(ctx, signal.NoCh(ctx))
}

func shutdownPProf(parent context.Context, pprofServer pprofShutdowner, logger logging.Logger) {
	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer shutdownCancel()
	if err := pprofServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("Unable to stop PProf", err)
	}
}

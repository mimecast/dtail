package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mimecast/dtail/internal/cli"
	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/handlers"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/jobs"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/server"
	"github.com/mimecast/dtail/internal/source"
	"github.com/mimecast/dtail/internal/user"
	"github.com/mimecast/dtail/internal/version"
)

type pprofShutdowner interface {
	Shutdown(context.Context) error
}

type profileServer interface {
	pprofShutdowner
	Address() string
	Start(*sync.WaitGroup)
}

type dserverService interface {
	Start(context.Context) (int, error)
}

type notifyContextFunc func(context.Context, ...os.Signal) (context.Context, context.CancelFunc)

type dserverLifecycleDependencies struct {
	stderr               io.Writer
	loggers              clients.LoggerDependencies
	enableProfilingRates func()
	newPProfServer       func(context.Context, string) (profileServer, error)
	newBackgroundJobs    func(config.RuntimeConfig, clients.LoggerDependencies) server.BackgroundJobs
	newServer            func(config.RuntimeConfig, handlers.HandlerLoggers, server.BackgroundJobs) (dserverService, error)
	notifyContext        notifyContextFunc
}

// The evil begins here.
func main() {
	os.Exit(run())
}

func run() int {
	var args config.Args
	var color bool
	var displayVersion bool
	var pprof string
	var shutdownAfter int

	if err := user.NoRootCheck(); err != nil {
		fmt.Fprintf(os.Stderr, "unable to start dserver: %v\n", err)
		return 1
	}

	flag.BoolVar(&color, "color", false, "Enable ANSII terminal colors")
	flag.BoolVar(&displayVersion, "version", false, "Display version")
	flag.IntVar(&args.SSHPort, "port", config.DefaultSSHPort, "SSH server port")
	flag.IntVar(&shutdownAfter, "shutdownAfter", 0, "Shutdown after so many seconds")
	flag.StringVar(&args.ConfigFile, "cfg", "", "Config file path")
	flag.StringVar(&args.HostnameOverride, "hostname-override", "", "Override the hostname used in logs and output")
	flag.StringVar(&args.LogDir, "logDir", "", "Log dir")
	flag.StringVar(&args.LogLevel, "logLevel", config.DefaultLogLevel, "Log level")
	flag.StringVar(&args.Logger, "logger", config.DefaultServerLogger, "Logger name")
	flag.StringVar(&args.SSHBindAddress, "bindAddress", "", "The SSH bind address")
	flag.StringVar(&args.AuthorizedKeysPath, "authorized-keys-path", "", "Authorized keys file path")
	flag.StringVar(&args.HostKeyPath, "host-key-path", "", "Private SSH host key path")
	flag.StringVar(&pprof, "pprof", "", "Start PProf server this address")

	flag.Parse()
	args.NoColor = !color
	runtimeCfg, err := config.SetupRuntime(source.Server, &args, flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to configure dserver: %v\n", err)
		return 1
	}

	if displayVersion {
		version.PrintAndExit(runtimeCfg.Client != nil && runtimeCfg.Client.TermColorsEnable)
	}
	version.Print(false)
	colorizer := brush.New(runtimeCfg.Client.TermColors)

	rootCtx, rootCancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	if err := dlog.Start(rootCtx, &wg, source.Server, runtimeCfg, colorizer); err != nil {
		wg.Done()
		rootCancel()
		fmt.Fprintf(os.Stderr, "unable to initialize dserver logger: %v\n", err)
		return 1
	}

	loggers := clients.NewLoggerDependencies(dlog.Client, dlog.Server, dlog.Common)
	return runDServerLifecycle(rootCtx, rootCancel, &wg, shutdownAfter, pprof,
		runtimeCfg, dserverLifecycleDependencies{
			stderr:               os.Stderr,
			loggers:              loggers,
			enableProfilingRates: cli.EnableProfilingRates,
			newPProfServer: func(ctx context.Context, address string) (profileServer, error) {
				return cli.NewPProfServer(ctx, address)
			},
			newBackgroundJobs: func(cfg config.RuntimeConfig, loggers clients.LoggerDependencies) server.BackgroundJobs {
				return jobs.New(cfg, loggers, colorizer)
			},
			newServer: func(cfg config.RuntimeConfig, loggers handlers.HandlerLoggers,
				backgroundJobs server.BackgroundJobs) (dserverService, error) {
				return server.New(cfg, loggers, backgroundJobs, colorizer)
			},
			notifyContext: signal.NotifyContext,
		})
}

func runDServerLifecycle(parent context.Context, cancel context.CancelFunc, wg *sync.WaitGroup,
	shutdownAfter int, pprofAddress string, cfg config.RuntimeConfig,
	deps dserverLifecycleDependencies) int {
	// Register logger cleanup first so logging stays available to every later
	// teardown step.
	defer func() {
		cancel()
		wg.Wait()
	}()

	if pprofAddress != "" {
		// Enable mutex and block profiling so the /debug/pprof/mutex and
		// /debug/pprof/block endpoints actually contain samples. These rates
		// are gated on --pprof so they cost nothing when profiling is off.
		deps.enableProfilingRates()

		pprofServer, pprofErr := deps.newPProfServer(parent, pprofAddress)
		if pprofErr != nil {
			deps.loggers.Client.Error("Unable to start PProf", pprofErr)
		} else {
			deps.loggers.Client.Info("Starting PProf", pprofServer.Address())
			pprofServer.Start(nil)
			defer shutdownPProf(parent, pprofServer, deps.loggers.Client)
		}
	}

	ctx, stopSignals := deps.notifyContext(parent, os.Interrupt, syscall.SIGTERM)
	timeoutCancel := context.CancelFunc(func() {})
	if shutdownAfter > 0 {
		ctx, timeoutCancel = context.WithTimeout(ctx, time.Duration(shutdownAfter)*time.Second)
	}
	// Registered last, this runs before pprof shutdown and logger draining so
	// signal delivery is restored even if either later step blocks.
	defer func() {
		stopSignals()
		timeoutCancel()
	}()

	var backgroundJobs server.BackgroundJobs
	if deps.newBackgroundJobs != nil {
		backgroundJobs = deps.newBackgroundJobs(cfg, deps.loggers)
	}
	serv, err := deps.newServer(cfg, handlers.HandlerLoggers{
		Diagnostics: deps.loggers.Server,
		Reader:      deps.loggers.Common,
	}, backgroundJobs)
	if err != nil {
		deps.loggers.Server.Error("Unable to initialize dserver", err)
		_, _ = fmt.Fprintf(deps.stderr, "unable to initialize dserver: %v\n", err)
		return 1
	}
	status, err := serv.Start(ctx)
	if err != nil {
		deps.loggers.Server.Error("Unable to run dserver", err)
		_, _ = fmt.Fprintf(deps.stderr, "unable to run dserver: %v\n", err)
		status = 1
	}
	return status
}

func shutdownPProf(parent context.Context, pprofServer pprofShutdowner, logger logging.Logger) {
	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer shutdownCancel()
	if err := pprofServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("Unable to stop PProf", err)
	}
}

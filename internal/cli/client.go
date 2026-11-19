package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/io/signal"
	"github.com/mimecast/dtail/internal/profiling"
	"github.com/mimecast/dtail/internal/source"
	"github.com/mimecast/dtail/internal/user"
	"github.com/mimecast/dtail/internal/version"
)

// ClientHook runs at a defined point in the common client lifecycle. A hook
// may handle the command itself by returning true and the desired exit status.
type ClientHook func(*config.Args) (handled bool, status int)

// ClientContextFactory creates the parent context used by a client runtime.
// The returned cancel function is called after the runtime has stopped.
type ClientContextFactory func(config.Args) (context.Context, context.CancelFunc)

// ClientBuilder constructs a client after its runtime and logger dependencies
// have been initialized.
type ClientBuilder func(config.Args, config.RuntimeConfig, clients.LoggerDependencies) (clients.Client, error)

// BrushClientBuilder constructs a client with its process-owned terminal brush.
type BrushClientBuilder func(config.Args, config.RuntimeConfig, clients.LoggerDependencies,
	*brush.Brush) (clients.Client, error)

// ClientRunner owns the shared flags and startup lifecycle of a client command.
type ClientRunner struct {
	fs                *flag.FlagSet
	args              *config.Args
	displayVersion    bool
	legacyAuthKeyPath string
	pprof             string
	profile           profiling.Flags
	beforeSetup       func(*config.Args)
	beforeRuntime     ClientHook
	afterRuntime      ClientHook
	contextFactory    ClientContextFactory
	brushFactory      func(config.TermColors) *brush.Brush
}

type clientRuntime interface {
	Context() context.Context
	Cancel()
	StartPProf(string)
	LogStartupMetrics()
	LogShutdownMetrics()
	Stop()
}

type clientRunDependencies struct {
	argv            []string
	stderr          io.Writer
	setup           func(source.Source, *config.Args, []string) (config.RuntimeConfig, error)
	currentUserName func() (string, error)
	printVersion    func(bool)
	newRuntime      func(context.Context, profiling.Flags, string) (clientRuntime, error)
	newBrushRuntime func(context.Context, profiling.Flags, string, config.RuntimeConfig,
		*brush.Brush) (clientRuntime, error)
	interrupt      func(context.Context, context.CancelFunc, time.Duration) <-chan string
	interruptPause time.Duration
	logBuildError  func(string, error)
	loggers        func() clients.LoggerDependencies
}

// BindCommonClientFlags registers flags shared by the interactive client
// commands and returns the runner that owns their parsed state.
func BindCommonClientFlags(fs *flag.FlagSet, args *config.Args) *ClientRunner {
	runner := &ClientRunner{fs: fs, args: args}
	fs.BoolVar(&args.NoColor, "noColor", false, "Disable ANSII terminal colors")
	fs.BoolVar(&args.NoAuthKey, "no-auth-key", false, "Disable auth-key fast reconnect feature")
	fs.BoolVar(&args.LogPayload, "log-payload", false, "Also tee retrieved payload into the client log file (default: file keeps diagnostics only)")
	fs.BoolVar(&args.Quiet, "quiet", false, "Quiet output mode")
	fs.BoolVar(&args.InteractiveQuery, "interactive-query", false, "Enable interactive in-flight query control over supported sessions")
	fs.BoolVar(&args.Plain, "plain", false, "Plain output mode")
	fs.BoolVar(&args.TrustAllHosts, "trustAllHosts", false, "Trust all unknown host keys")
	fs.BoolVar(&runner.displayVersion, "version", false, "Display version")
	fs.IntVar(&args.ConnectionsPerCPU, "cpc", config.DefaultConnectionsPerCPU,
		"How many connections established per CPU core concurrently")
	fs.IntVar(&args.SSHAgentKeyIndex, "agentKeyIndex", -1, "SSH agent key index to use (-1 for all keys)")
	fs.IntVar(&args.SSHPort, "port", config.DefaultSSHPort, "SSH server port")
	fs.StringVar(&args.ConfigFile, "cfg", "", "Config file path")
	fs.StringVar(&args.ControlTTYPath, "control-tty", "/dev/tty", "TTY device for interactive query control")
	fs.StringVar(&args.Discovery, "discovery", "", "Server discovery method")
	fs.StringVar(&args.HostnameOverride, "hostname-override", "", "Override the hostname used in logs and output")
	fs.StringVar(&args.LogDir, "logDir", "~/log", "Log dir")
	fs.StringVar(&args.Logger, "logger", config.DefaultClientLogger, "Logger name")
	fs.StringVar(&args.LogLevel, "logLevel", config.DefaultLogLevel, "Log level")
	BindAuthKeyFlags(fs, &runner.legacyAuthKeyPath, args)
	fs.StringVar(&args.KnownHostsPath, "known-hosts-path", "", "OpenSSH known_hosts file path")
	fs.StringVar(&args.ServersStr, "servers", "", "Remote servers to connect")
	fs.StringVar(&args.UserName, "user", "", "Your system user name")
	fs.StringVar(&args.What, "files", "", "File(s) to read")
	fs.StringVar(&runner.pprof, "pprof", "", "Start PProf server this address")
	profiling.BindFlags(fs, &runner.profile)
	return runner
}

// BeforeSetup sets an optional normalization hook that runs after parsing and
// auth-key compatibility handling, immediately before config.SetupRuntime.
func (r *ClientRunner) BeforeSetup(hook func(*config.Args)) *ClientRunner {
	r.beforeSetup = hook
	return r
}

// BeforeRuntime sets an optional hook that runs after setup, version handling,
// and user lookup, but before the runtime starts.
func (r *ClientRunner) BeforeRuntime(hook ClientHook) *ClientRunner {
	r.beforeRuntime = hook
	return r
}

// AfterRuntime sets an optional hook that runs after the runtime starts and
// before profiling and client construction.
func (r *ClientRunner) AfterRuntime(hook ClientHook) *ClientRunner {
	r.afterRuntime = hook
	return r
}

// WithContext sets the factory for the runtime's parent context.
func (r *ClientRunner) WithContext(factory ClientContextFactory) *ClientRunner {
	r.contextFactory = factory
	return r
}

// WithBrushFactory sets the terminal brush constructor selected by the command.
func (r *ClientRunner) WithBrushFactory(factory func(config.TermColors) *brush.Brush) *ClientRunner {
	r.brushFactory = factory
	return r
}

// RunClient parses and configures a client command, owns its runtime, and
// returns the process exit status.
func (r *ClientRunner) RunClient(name string, build ClientBuilder) int {
	deps := clientRunDependencies{
		argv:            os.Args[1:],
		stderr:          os.Stderr,
		setup:           config.SetupRuntime,
		currentUserName: user.CurrentName,
		printVersion:    version.Print,
		newBrushRuntime: func(ctx context.Context, flags profiling.Flags, name string,
			cfg config.RuntimeConfig, colorizer *brush.Brush) (clientRuntime, error) {
			return NewClientRuntime(ctx, flags, name, cfg, colorizer)
		},
		interrupt:      signal.InterruptChWithCancel,
		interruptPause: time.Second * time.Duration(config.InterruptTimeoutS),
		logBuildError: func(name string, err error) {
			dlog.Client.Error("Unable to create "+name+" client", err)
		},
		loggers: func() clients.LoggerDependencies {
			return clients.NewLoggerDependencies(dlog.Client, dlog.Server, dlog.Common)
		},
	}
	return r.runClient(name, build, deps)
}

func (r *ClientRunner) runClient(name string,
	build ClientBuilder, deps clientRunDependencies) int {
	return r.runClientWithBrush(name, func(args config.Args, cfg config.RuntimeConfig,
		loggers clients.LoggerDependencies, _ *brush.Brush) (clients.Client, error) {
		return build(args, cfg, loggers)
	}, deps)
}

// RunClientWithBrush runs a client whose constructor accepts the command-owned brush.
func (r *ClientRunner) RunClientWithBrush(name string, build BrushClientBuilder) int {
	deps := clientRunDependencies{
		argv:            os.Args[1:],
		stderr:          os.Stderr,
		setup:           config.SetupRuntime,
		currentUserName: user.CurrentName,
		printVersion:    version.Print,
		newBrushRuntime: func(ctx context.Context, flags profiling.Flags, name string,
			cfg config.RuntimeConfig, colorizer *brush.Brush) (clientRuntime, error) {
			return NewClientRuntime(ctx, flags, name, cfg, colorizer)
		},
		interrupt:      signal.InterruptChWithCancel,
		interruptPause: time.Second * time.Duration(config.InterruptTimeoutS),
		logBuildError: func(name string, err error) {
			dlog.Client.Error("Unable to create "+name+" client", err)
		},
		loggers: func() clients.LoggerDependencies {
			return clients.NewLoggerDependencies(dlog.Client, dlog.Server, dlog.Common)
		},
	}
	return r.runClientWithBrush(name, build, deps)
}

func (r *ClientRunner) runClientWithBrush(name string,
	build BrushClientBuilder, deps clientRunDependencies) int {
	if err := r.fs.Parse(deps.argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if warning := ApplyAuthKeyPathCompatibility(r.args, r.legacyAuthKeyPath,
		FlagWasSet(r.fs, "auth-key-path")); warning != "" {
		_, _ = fmt.Fprintln(deps.stderr, warning)
	}
	if r.beforeSetup != nil {
		r.beforeSetup(r.args)
	}
	runtimeCfg, err := deps.setup(source.Client, r.args, r.fs.Args())
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "unable to configure %s: %v\n", name, err)
		return 1
	}
	if r.displayVersion {
		colorsEnabled := runtimeCfg.Client != nil && runtimeCfg.Client.TermColorsEnable
		deps.printVersion(colorsEnabled)
		return 0
	}
	if r.args.UserName == "" {
		userName, userErr := deps.currentUserName()
		if userErr != nil {
			_, _ = fmt.Fprintf(deps.stderr, "unable to determine %s user: %v\n", name, userErr)
			return 1
		}
		r.args.UserName = userName
	}
	if r.beforeRuntime != nil {
		if handled, status := r.beforeRuntime(r.args); handled {
			return status
		}
	}

	var parentCtx context.Context
	var cancel context.CancelFunc
	if r.contextFactory == nil {
		parentCtx, cancel = context.WithCancel(context.Background())
	} else {
		parentCtx, cancel = r.contextFactory(*r.args)
		if parentCtx == nil {
			if cancel != nil {
				cancel()
			}
			_, _ = fmt.Fprintf(deps.stderr, "unable to initialize %s runtime: context factory returned nil context\n", name)
			return 1
		}
		if cancel == nil {
			_, _ = fmt.Fprintf(deps.stderr, "unable to initialize %s runtime: context factory returned nil cancel function\n", name)
			return 1
		}
	}
	defer cancel()

	theme := config.DefaultTermColors()
	if runtimeCfg.Client != nil {
		theme = runtimeCfg.Client.TermColors
	}
	brushFactory := r.brushFactory
	if brushFactory == nil {
		brushFactory = brush.New
	}
	colorizer := brushFactory(theme)

	var runtime clientRuntime
	if deps.newBrushRuntime != nil {
		runtime, err = deps.newBrushRuntime(parentCtx, r.profile, name, runtimeCfg, colorizer)
	} else {
		runtime, err = deps.newRuntime(parentCtx, r.profile, name)
	}
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "unable to initialize %s runtime: %v\n", name, err)
		return 1
	}
	defer runtime.Stop()

	if r.afterRuntime != nil {
		if handled, status := r.afterRuntime(r.args); handled {
			return status
		}
	}
	runtime.StartPProf(r.pprof)
	runtime.LogStartupMetrics()

	loggers := clients.LoggerDependencies{}
	if deps.loggers != nil {
		loggers = deps.loggers()
	}
	client, err := build(*r.args, runtimeCfg, loggers, colorizer)
	if err != nil {
		deps.logBuildError(name, err)
		_, _ = fmt.Fprintf(deps.stderr, "unable to create %s client: %v\n", name, err)
		return 1
	}

	status := client.Start(runtime.Context(), deps.interrupt(runtime.Context(), runtime.Cancel,
		deps.interruptPause))
	runtime.LogShutdownMetrics()
	return status
}

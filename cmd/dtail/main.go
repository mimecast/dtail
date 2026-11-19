package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/mimecast/dtail/internal/cli"
	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

func main() {
	os.Exit(run())
}

func run() int {
	args := config.Args{Mode: omode.TailClient}
	runner := cli.BindCommonClientFlags(flag.CommandLine, &args)
	runner.WithBrushFactory(brush.New)
	options := bindTailFlags(flag.CommandLine, &args)
	options.configureRunner(runner)
	return runner.RunClientWithBrush("dtail", buildTailClient)
}

type tailOptions struct {
	checkHealth           bool
	displayColorTable     bool
	displayWideColorTable bool
	grep                  string
	shutdownAfter         int
}

func bindTailFlags(fs *flag.FlagSet, args *config.Args) *tailOptions {
	options := &tailOptions{}
	fs.BoolVar(&args.RegexInvert, "invert", false, "Invert regex")
	fs.BoolVar(&options.checkHealth, "checkHealth", false, "Deprecated, flag will be removed soon")
	fs.BoolVar(&options.displayColorTable, "colorTable", false, "Show color table")
	fs.BoolVar(&options.displayWideColorTable, "wideColorTable", false, "Show a large color table")
	fs.IntVar(&args.AfterContext, "after", 0, "Print lines of trailing context after matching lines")
	fs.IntVar(&args.BeforeContext, "before", 0, "Print lines of leading context before matching lines")
	fs.IntVar(&args.MaxCount, "max", 0, "Stop reading file after NUM matching lines")
	fs.IntVar(&args.Timeout, "timeout", 0, "Max time dtail server will collect data until disconnection")
	fs.IntVar(&options.shutdownAfter, "shutdownAfter", 3600*24, "Shutdown after so many seconds")
	fs.StringVar(&args.QueryStr, "query", "", "Map reduce query")
	fs.StringVar(&args.RegexStr, "regex", ".", "Regular expression")
	fs.StringVar(&options.grep, "grep", "", "Alias for -regex")
	return options
}

func (o *tailOptions) configureRunner(runner *cli.ClientRunner) {
	runner.BeforeSetup(func(args *config.Args) {
		if o.grep != "" {
			args.RegexStr = o.grep
		}
	}).BeforeRuntime(func(args *config.Args) (bool, int) {
		if args.Plain {
			return false, 0
		}
		if o.displayWideColorTable {
			color.PrintTable(true)
			return true, 0
		}
		if o.displayColorTable {
			color.PrintTable(false)
			return true, 0
		}
		return false, 0
	}).WithContext(func(args config.Args) (context.Context, context.CancelFunc) {
		return applyClientDeadlines(context.Background(), o.shutdownAfter, args.Timeout)
	}).AfterRuntime(func(*config.Args) (bool, int) {
		if !o.checkHealth {
			return false, 0
		}
		fmt.Println("WARN: DTail health check has moved to separate binary dtailhealth" +
			" - please adjust the monitoring scripts!")
		return true, 1
	})
}

func buildTailClient(args config.Args, cfg config.RuntimeConfig, loggers clients.LoggerDependencies,
	colorizer *brush.Brush) (clients.Client, error) {
	if args.QueryStr == "" {
		return clients.NewTailClient(args, cfg, loggers, colorizer)
	}
	return clients.NewMaprClient(args, cfg, clients.DefaultMode, loggers, colorizer)
}

// applyClientDeadlines wraps ctx with the earliest of two absolute deadlines:
// the --shutdownAfter safety cap and the user's --timeout data-collection
// window. Whichever elapses first cancels the returned context, which propagates
// through the follow client's reconnect and per-connection read loops (both
// select on ctx.Done()), so client.Start returns cleanly and the process exits
// instead of auto-reconnecting for another cycle.
//
// Making --timeout a client-side deadline (rather than relying solely on the
// server closing the read) is what fixes the historical hang: the server-side
// read deadline fires at N seconds, but in tail+query mode the session stays
// alive via the map/aggregate command, so the client used to treat the closed
// read as a transient drop and reconnect indefinitely. A client-side deadline
// makes --timeout behave consistently for follows with or without --query, which
// matches the flag's help text ("Max time ... until disconnection").
//
// The two deadlines compose naturally (context deadlines nest, so the earlier
// one wins), so they never conflict with each other. A timeout of 0 (unset)
// contributes no deadline, preserving the previous behaviour. OS signals reach
// the same context via signal.InterruptChWithCancel in the shared client runner.
func applyClientDeadlines(ctx context.Context, shutdownAfter, timeout int) (
	context.Context, context.CancelFunc) {
	var cancels []context.CancelFunc
	addDeadline := func(seconds int) {
		if seconds <= 0 {
			return
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
		cancels = append(cancels, cancel)
	}

	addDeadline(shutdownAfter)
	addDeadline(timeout)

	return ctx, func() {
		for i := len(cancels) - 1; i >= 0; i-- {
			cancels[i]()
		}
	}
}

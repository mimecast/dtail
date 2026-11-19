package main

import (
	"flag"
	"os"

	"github.com/mimecast/dtail/internal/cli"
	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
)

func main() {
	os.Exit(run())
}

func run() int {
	var args config.Args
	var grep string
	runner := cli.BindCommonClientFlags(flag.CommandLine, &args)
	runner.WithBrushFactory(brush.New)
	flag.BoolVar(&args.RegexInvert, "invert", false, "Invert regex")
	flag.IntVar(&args.AfterContext, "after", 0, "Print lines of trailing context after matching lines")
	flag.IntVar(&args.BeforeContext, "before", 0, "Print lines of leading context before matching lines")
	flag.IntVar(&args.MaxCount, "max", 0, "Stop reading file after NUM matching lines")
	flag.StringVar(&args.RegexStr, "regex", ".", "Regular expression")
	flag.StringVar(&grep, "grep", "", "Alias for -regex")
	runner.BeforeSetup(func(args *config.Args) {
		if grep != "" {
			args.RegexStr = grep
		}
	})
	return runner.RunClientWithBrush("dgrep", func(args config.Args, cfg config.RuntimeConfig,
		loggers clients.LoggerDependencies, colorizer *brush.Brush) (clients.Client, error) {
		return clients.NewGrepClient(args, cfg, loggers, colorizer)
	})
}

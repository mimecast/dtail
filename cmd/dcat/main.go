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
	runner := cli.BindCommonClientFlags(flag.CommandLine, &args)
	runner.WithBrushFactory(brush.New)
	return runner.RunClientWithBrush("dcat", func(args config.Args, cfg config.RuntimeConfig,
		loggers clients.LoggerDependencies, colorizer *brush.Brush) (clients.Client, error) {
		return clients.NewCatClient(args, cfg, loggers, colorizer)
	})
}

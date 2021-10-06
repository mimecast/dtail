package main

import (
	"context"
	"flag"
	"os"
	"sync"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/io/signal"
	"github.com/mimecast/dtail/internal/source"
)

// The evil begins here.
func main() {
	var args config.Args
	flag.StringVar(&args.ServersStr, "server", "", "Remote server to connect")
	flag.Parse()
	config.Setup(source.HealthCheck, &args, flag.Args())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)

	dlog.Start(ctx, &wg, source.HealthCheck, config.Common.LogLevel)
	healthClient, _ := clients.NewHealthClient(args)
	os.Exit(healthClient.Start(ctx, signal.NoCh(ctx)))
}

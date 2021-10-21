package main

import (
	"context"
	"flag"
	"os"
	"sync"

	"net/http"
	_ "net/http"
	_ "net/http/pprof"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/io/signal"
	"github.com/mimecast/dtail/internal/source"
	"github.com/mimecast/dtail/internal/version"
)

// The evil begins here.
func main() {
	var args config.Args
	var displayVersion bool
	var pprof string

	flag.BoolVar(&displayVersion, "version", false, "Display version")
	flag.StringVar(&args.Logger, "logger", config.DefaultHealthCheckLogger, "Logger name")
	flag.StringVar(&args.LogLevel, "logLevel", "none", "Log level")
	flag.StringVar(&args.ServersStr, "server", "", "Remote server to connect")
	flag.StringVar(&pprof, "pprof", "", "Start PProf server this address")
	flag.Parse()

	if displayVersion {
		version.PrintAndExit()
	}

	config.Setup(source.HealthCheck, &args, flag.Args())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	dlog.Start(ctx, &wg, source.HealthCheck)

	if pprof != "" {
		go http.ListenAndServe(pprof, nil)
		dlog.Client.Info("Started PProf", pprof)
	}

	healthClient, _ := clients.NewHealthClient(args)
	os.Exit(healthClient.Start(ctx, signal.NoCh(ctx)))
}

package main

import (
	"context"
	"flag"
	"fmt"
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
)

// The evil begins here.
func main() {
	var args config.Args
	var pprof int

	flag.IntVar(&pprof, "pprof", -1, "Start PProf server this port")
	flag.StringVar(&args.Logger, "logger", config.DefaultHealthCheckLogger, "Logger name")
	flag.StringVar(&args.LogLevel, "logLevel", "none", "Log level")
	flag.StringVar(&args.ServersStr, "server", "", "Remote server to connect")
	flag.Parse()

	config.Setup(source.HealthCheck, &args, flag.Args())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	dlog.Start(ctx, &wg, source.HealthCheck)

	if pprof > -1 {
		// For debugging purposes only
		pprofArgs := fmt.Sprintf("0.0.0.0:%d", pprof)
		go http.ListenAndServe(pprofArgs, nil)
		dlog.Client.Info("Started PProf", pprofArgs)
	}

	healthClient, _ := clients.NewHealthClient(args)
	os.Exit(healthClient.Start(ctx, signal.NoCh(ctx)))
}

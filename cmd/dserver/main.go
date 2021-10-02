package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	_ "net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/server"
	"github.com/mimecast/dtail/internal/source"
	"github.com/mimecast/dtail/internal/user"
	"github.com/mimecast/dtail/internal/version"
)

// The evil begins here.
func main() {
	var args config.Args
	var color bool
	var displayVersion bool
	var pprof int
	var shutdownAfter int

	user.NoRootCheck()

	flag.BoolVar(&color, "color", false, "Enable ANSII terminal colors")
	flag.BoolVar(&config.ServerRelaxedAuthEnable, "relaxedAuth", false, "Enable relaxced SSH auth mode (don't use in production!)")
	flag.BoolVar(&displayVersion, "version", false, "Display version")
	flag.IntVar(&args.SSHPort, "port", config.DefaultSSHPort, "SSH server port")
	flag.IntVar(&pprof, "pprof", -1, "Start PProf server this port")
	flag.IntVar(&shutdownAfter, "shutdownAfter", 0, "Shutdown after so many seconds")
	flag.StringVar(&args.ConfigFile, "cfg", "", "Config file path")
	flag.StringVar(&args.LogDir, "logDir", "", "Log dir")
	flag.StringVar(&args.LogLevel, "logLevel", "", "Log level")

	flag.Parse()
	args.NoColor = !color
	config.Setup(&args, flag.Args())

	if displayVersion {
		version.PrintAndExit()
	}
	version.Print()

	ctx, cancel := context.WithCancel(context.Background())
	if shutdownAfter > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(shutdownAfter)*time.Second)
	}

	sigCh := make(chan os.Signal)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	dlog.Start(ctx, &wg, source.Server, config.Common.LogLevel)

	if config.ServerRelaxedAuthEnable {
		dlog.Server.Fatal("SSH relaxed-auth mode enabled")
	}

	if pprof > -1 {
		// Start of pprof server for development purposes only.
		pprofArgs := fmt.Sprintf("0.0.0.0:%d", pprof)
		dlog.Server.Info("Starting PProf", pprofArgs)
		go http.ListenAndServe(pprofArgs, nil)
	}

	serv := server.New()
	status := serv.Start(ctx)
	cancel()

	wg.Wait()
	os.Exit(status)
}

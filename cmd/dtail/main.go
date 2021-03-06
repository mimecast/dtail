package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	_ "net/http"
	_ "net/http/pprof"
	"os"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/io/signal"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/user"
	"github.com/mimecast/dtail/internal/version"
)

// The evil begins here.
func main() {
	var args clients.Args
	var cfgFile string
	var checkHealth bool
	var debugEnable bool
	var displayVersion bool
	var grep string
	var noColor bool
	var pprof int
	var queryStr string
	var shutdownAfter int
	var sshPort int

	userName := user.Name()

	flag.BoolVar(&args.RegexInvert, "invert", false, "Invert regex")
	flag.BoolVar(&args.TrustAllHosts, "trustAllHosts", false, "Auto trust all unknown host keys")
	flag.BoolVar(&checkHealth, "checkHealth", false, "Only check for server health")
	flag.BoolVar(&debugEnable, "debug", false, "Activate debug messages")
	flag.BoolVar(&displayVersion, "version", false, "Display version")
	flag.BoolVar(&noColor, "noColor", false, "Disable ANSII terminal colors")
	flag.BoolVar(&args.Quiet, "quiet", false, "Quiet output mode")
	flag.IntVar(&args.ConnectionsPerCPU, "cpc", 10, "How many connections established per CPU core concurrently")
	flag.IntVar(&args.Timeout, "timeout", 0, "Max time dtail server will collect data until disconnection")
	flag.IntVar(&pprof, "pprof", -1, "Start PProf server this port")
	flag.IntVar(&shutdownAfter, "shutdownAfter", 3600*24, "Automatically shutdown after so many seconds")
	flag.IntVar(&sshPort, "port", 2222, "SSH server port")
	flag.StringVar(&args.Discovery, "discovery", "", "Server discovery method")
	flag.StringVar(&args.PrivateKeyPathFile, "key", "", "Path to private key")
	flag.StringVar(&args.ServersStr, "servers", "", "Remote servers to connect")
	flag.StringVar(&args.UserName, "user", userName, "Your system user name")
	flag.StringVar(&args.What, "files", "", "File(s) to read")
	flag.StringVar(&cfgFile, "cfg", "", "Config file path")
	flag.StringVar(&queryStr, "query", "", "Map reduce query")

	// Line context awareness.
	flag.StringVar(&args.RegexStr, "regex", ".", "Regular expression")
	flag.StringVar(&grep, "grep", "", "Alias for -regex")
	flag.IntVar(&args.LContext.BeforeContext, "before", 0, "Print lines of leading context before matching lines")
	flag.IntVar(&args.LContext.AfterContext, "after", 0, "Print lines of trailing context after matching lines")
	flag.IntVar(&args.LContext.MaxCount, "max", 0, "Stop reading file after NUM matching lines")

	flag.Parse()

	if grep != "" {
		args.RegexStr = grep
	}

	config.Read(cfgFile, sshPort)
	color.Colored = !noColor

	if displayVersion {
		version.PrintAndExit()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if shutdownAfter > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(shutdownAfter)*time.Second)
		defer cancel()
	}

	if checkHealth {
		healthClient, _ := clients.NewHealthClient(omode.HealthClient)
		os.Exit(healthClient.Start(ctx))
	}

	logger.Start(ctx, logger.Modes{
		Debug: debugEnable || config.Common.DebugEnable,
		Quiet: args.Quiet,
	})

	if pprof > -1 {
		// For debugging purposes only
		pprofArgs := fmt.Sprintf("0.0.0.0:%d", pprof)
		logger.Info("Starting PProf", pprofArgs)
		go http.ListenAndServe(pprofArgs, nil)
	}

	var client clients.Client
	var err error
	args.Mode = omode.TailClient

	switch queryStr {
	case "":
		if client, err = clients.NewTailClient(args); err != nil {
			panic(err)
		}
	default:
		if client, err = clients.NewMaprClient(args, queryStr, clients.DefaultMode); err != nil {
			panic(err)
		}
	}

	status := client.Start(ctx, signal.InterruptCh(ctx))
	logger.Flush()
	os.Exit(status)
}

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
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/user"
	"github.com/mimecast/dtail/internal/version"
)

// The evil begins here.
func main() {
	var cfgFile string
	var checkHealth bool
	var connectionsPerCPU int
	var debugEnable bool
	var discovery string
	var shutdownAfter int
	var pprof int
	var displayVersion bool
	var files string
	var noColor bool
	var queryStr string
	var regexStr string
	var regexInvert bool
	var serversStr string
	var quietEnable bool
	var sshPort int
	var timeout int
	var trustAllHosts bool
	var privateKeyPathFile string

	userName := user.Name()

	flag.BoolVar(&checkHealth, "checkHealth", false, "Only check for server health")
	flag.BoolVar(&debugEnable, "debug", false, "Activate debug messages")
	flag.BoolVar(&displayVersion, "version", false, "Display version")
	flag.BoolVar(&noColor, "noColor", false, "Disable ANSII terminal colors")
	flag.IntVar(&shutdownAfter, "shutdownAfter", 3600*24, "Automatically shutdown after so many seconds")
	flag.BoolVar(&quietEnable, "quiet", false, "Reduce output")
	flag.BoolVar(&trustAllHosts, "trustAllHosts", false, "Auto trust all unknown host keys")
	flag.IntVar(&connectionsPerCPU, "cpc", 10, "How many connections established per CPU core concurrently")
	flag.IntVar(&sshPort, "port", 2222, "SSH server port")
	flag.IntVar(&timeout, "timeout", 0, "Max time dtail server will collect data until disconnection")
	flag.IntVar(&pprof, "pprof", -1, "Start PProf server this port")
	flag.StringVar(&cfgFile, "cfg", "", "Config file path")
	flag.StringVar(&discovery, "discovery", "", "Server discovery method")
	flag.StringVar(&files, "files", "", "File(s) to read")
	flag.StringVar(&queryStr, "query", "", "Map reduce query")
	flag.StringVar(&regexStr, "regex", ".", "Regular expression")
	flag.StringVar(&regexStr, "grep", ".", "Alias for -regex")
	flag.BoolVar(&regexInvert, "invert", false, "Invert regex")
	flag.StringVar(&serversStr, "servers", "", "Remote servers to connect")
	flag.StringVar(&userName, "user", userName, "Your system user name")
	flag.StringVar(&privateKeyPathFile, "key", "", "Path to private key")

	flag.Parse()

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

	logger.Start(ctx, logger.Modes{Debug: debugEnable || config.Common.DebugEnable, Quiet: quietEnable})

	if pprof > -1 {
		// For debugging purposes only
		pprofArgs := fmt.Sprintf("0.0.0.0:%d", pprof)
		logger.Info("Starting PProf", pprofArgs)
		go http.ListenAndServe(pprofArgs, nil)
	}

	args := clients.Args{
		ConnectionsPerCPU:  connectionsPerCPU,
		ServersStr:         serversStr,
		Discovery:          discovery,
		UserName:           userName,
		What:               files,
		TrustAllHosts:      trustAllHosts,
		RegexStr:           regexStr,
		RegexInvert:        regexInvert,
		Mode:               omode.TailClient,
		Timeout:            timeout,
		PrivateKeyPathFile: privateKeyPathFile,
	}

	var client clients.Client
	var err error

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

	status := client.Start(ctx)
	logger.Flush()
	os.Exit(status)
}

package main

import (
	"dtail/clients"
	"dtail/color"
	"dtail/config"
	"dtail/logger"
	"dtail/omode"
	"dtail/server"
	"dtail/version"
	"flag"
	"fmt"
	"net/http"
	_ "net/http"
	_ "net/http/pprof"
	"os"
	"os/user"
	"runtime"
	"sync"
	"time"
)

// The evil begins here.
func main() {
	var cfgFile, modeStr string
	var checkHealth bool
	var clientServerEnable bool
	var connectionsPerCPU int
	var debugEnable bool
	var discovery string
	var displayVersion bool
	var files string
	var grep, regex string
	var maxInitConnections int
	var noColor bool
	var pprofEnable bool
	var queryStr string
	var serversStr string
	var shutdownAfter int
	var silent bool
	var sshPort int
	var trustAllHosts bool
	var userName string

	user, err := user.Current()
	if err != nil {
		panic(err)
	}

	if user.Uid == "0" {
		panic("Not allowed to run as UID 0")
	}

	if user.Gid == "0" {
		panic("Not allowed to run as GID 0")
	}

	defaultMode := omode.Default()
	serverEnable := defaultMode == omode.Server
	clientEnable := !serverEnable

	// Based on the mode we have different default timeouts
	var pingTimeoutS int
	switch defaultMode {
	case omode.CatClient:
		fallthrough
	case omode.GrepClient:
		pingTimeoutS = 60
	case omode.MapClient:
		pingTimeoutS = 900
	default:
		pingTimeoutS = 5
	}

	flag.BoolVar(&checkHealth, "checkHealth", false, "Only check for server health")
	flag.BoolVar(&clientServerEnable, "clientServer", false, "Enable client and server (dev purposes)")
	flag.BoolVar(&debugEnable, "debug", false, "Activate debug messages")
	flag.BoolVar(&displayVersion, "version", false, "Display version")
	flag.BoolVar(&noColor, "noColor", false, "Disable ANSII terminal colors")
	flag.BoolVar(&pprofEnable, "pprofEnable", false, "Enable pprof server")
	flag.BoolVar(&serverEnable, "server", serverEnable, "Start as a DTail server")
	flag.BoolVar(&silent, "silent", false, "Reduce output")
	flag.BoolVar(&trustAllHosts, "trustAllHosts", false, "Auto trust all unknown host keys")
	flag.IntVar(&connectionsPerCPU, "cpc", 10, "How many connections established per CPU core concurrently")
	flag.IntVar(&maxInitConnections, "mic", 20, "Max cpc")
	flag.IntVar(&shutdownAfter, "shutdownAfter", 0, "Automatically shutdown after so many seconds")
	flag.IntVar(&sshPort, "port", 2222, "SSH server port")
	flag.IntVar(&pingTimeoutS, "pingTimeout", 10, "The server ping timeout (0 means disable pings)")
	flag.StringVar(&cfgFile, "cfg", "", "Config file path")
	flag.StringVar(&discovery, "discovery", "", "Server discovery method")
	flag.StringVar(&files, "files", "", "File(s) to read")
	flag.StringVar(&grep, "grep", "", "Regular expression (deprecated)")
	flag.StringVar(&modeStr, "mode", defaultMode.String(), "Operating mode (tail, grep, cat, map, server)")
	flag.StringVar(&queryStr, "query", "", "Map reduce query")
	flag.StringVar(&regex, "regex", "", "Regular expression")
	flag.StringVar(&serversStr, "servers", "", "Remote servers to connect")
	flag.StringVar(&userName, "user", user.Username, "Your system user name")

	mode := omode.New(modeStr)

	flag.Parse()

	config.Init(cfgFile)
	color.Init(!noColor)

	if displayVersion {
		fmt.Println(version.PaintedString())
		os.Exit(0)
	}

	// Figure out how many SSH sessions can be established concurrently.
	if connectionsPerCPU*runtime.NumCPU() < maxInitConnections {
		maxInitConnections = connectionsPerCPU * runtime.NumCPU()
	}

	// Figure out in which mode I am? Server or client or both (the latter for dev purposes)?
	if serverEnable {
		clientEnable = false
	}
	if clientServerEnable {
		clientEnable = true
		serverEnable = true
	}

	// If non-standard port specified, overwrite config
	if sshPort != 2222 {
		config.Common.SSHPort = sshPort
	}

	// Figure out the log level.
	var logMode logger.LogMode
	switch {
	case debugEnable:
		logMode = logger.DebugMode
	case checkHealth:
		logMode = logger.NothingMode
	case config.Common.TraceEnable:
		logMode = logger.TraceMode
	case config.Common.DebugEnable:
		logMode = logger.DebugMode
	case silent:
		logMode = logger.SilentMode
	default:
		logMode = logger.NormalMode
	}

	// Figure out the log strategy.
	var logStrategy logger.LogStrategy
	switch config.Common.LogStrategy {
	case "daily":
		logStrategy = logger.DailyStrategy
	case "stdout":
		fallthrough
	default:
		logStrategy = logger.StdoutStrategy
	}

	logger.Init(serverEnable, logMode, logStrategy)

	// Wait group for shutting down logger.
	var wg sync.WaitGroup
	if serverEnable {
		wg.Add(1)
	}
	if clientEnable {
		wg.Add(1)
	}

	logger.Debug("Common config", config.Common)
	logger.Debug("Client config", config.Client)
	logger.Debug("Server config", config.Server)

	if grep != "" {
		logger.Warn("Flag 'grep' is deprecated and may be removed in the future, please use 'regex' instead")
		if regex == "" {
			regex = grep
		}
	}

	if checkHealth {
		healthClient, _ := clients.NewHealthClient(omode.HealthClient)
		os.Exit(healthClient.Start(&wg))
	}

	if shutdownAfter > 0 {
		go func() {
			defer os.Exit(1)

			logger.Info("Enabling auto shutdown timer", shutdownAfter)
			time.Sleep(time.Duration(shutdownAfter) * time.Second)
			logger.Info("Auto shutdown timer reached, shutting down now")
		}()
	}

	if pprofEnable || config.Common.PProfEnable {
		bindAddr := fmt.Sprintf("%s:%d", config.Common.PProfBindAddress, config.Common.PProfPort)
		logger.Info("Starting PProf server", bindAddr)
		go http.ListenAndServe(bindAddr, nil)
	}

	if serverEnable {
		logger.Info("Launching server", mode, version.String())
		sshServer := server.New()
		go sshServer.Start(&wg)
	}

	if clientEnable {
		var client clients.Client
		var err error

		logger.Info("Launching client", mode, version.String())

		args := clients.Args{
			Mode:               mode,
			ServersStr:         serversStr,
			Discovery:          discovery,
			UserName:           userName,
			Files:              files,
			Regex:              regex,
			TrustAllHosts:      trustAllHosts,
			MaxInitConnections: maxInitConnections,
			PingTimeout:        pingTimeoutS,
		}

		switch mode {
		case omode.TailClient:
			switch queryStr {
			case "":
				client, err = clients.NewTailClient(args)
			default:
				client, err = clients.NewMaprClient(args, queryStr)
			}
		case omode.GrepClient:
			client, err = clients.NewGrepClient(args)
		case omode.CatClient:
			client, err = clients.NewCatClient(args)
		case omode.MapClient:
			client, err = clients.NewMaprClient(args, queryStr)
		}

		if err != nil {
			panic(err)
		}

		go client.Start(&wg)
	}

	wg.Wait()
	logger.Stop()
}

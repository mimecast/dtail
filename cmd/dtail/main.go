package main

import (
	"flag"
	"os"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logger"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/pprof"
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
	var displayVersion bool
	var files string
	var noColor bool
	var pprofEnable bool
	var queryStr string
	var regex string
	var serversStr string
	var silentEnable bool
	var sshPort int
	var trustAllHosts bool

	pingTimeoutS := 5
	userName := user.Name()

	flag.BoolVar(&checkHealth, "checkHealth", false, "Only check for server health")
	flag.BoolVar(&debugEnable, "debug", false, "Activate debug messages")
	flag.BoolVar(&displayVersion, "version", false, "Display version")
	flag.BoolVar(&noColor, "noColor", false, "Disable ANSII terminal colors")
	flag.BoolVar(&pprofEnable, "pprofEnable", false, "Enable pprof server")
	flag.BoolVar(&silentEnable, "silent", false, "Reduce output")
	flag.BoolVar(&trustAllHosts, "trustAllHosts", false, "Auto trust all unknown host keys")
	flag.IntVar(&connectionsPerCPU, "cpc", 10, "How many connections established per CPU core concurrently")
	flag.IntVar(&pingTimeoutS, "pingTimeout", 10, "The server ping timeout (0 means disable pings)")
	flag.IntVar(&sshPort, "port", 2222, "SSH server port")
	flag.StringVar(&cfgFile, "cfg", "", "Config file path")
	flag.StringVar(&discovery, "discovery", "", "Server discovery method")
	flag.StringVar(&files, "files", "", "File(s) to read")
	flag.StringVar(&queryStr, "query", "", "Map reduce query")
	flag.StringVar(&regex, "regex", ".", "Regular expression")
	flag.StringVar(&serversStr, "servers", "", "Remote servers to connect")
	flag.StringVar(&userName, "user", userName, "Your system user name")

	flag.Parse()

	config.Read(cfgFile, sshPort)
	color.Colored = !noColor

	if displayVersion {
		version.PrintAndExit()
	}

	if checkHealth {
		healthClient, _ := clients.NewHealthClient(omode.HealthClient)
		os.Exit(healthClient.Start())
	}

	serverEnable := false
	if checkHealth {
		silentEnable = true
	}
	logger.Start(serverEnable, debugEnable, silentEnable, silentEnable)
	defer logger.Stop()

	if pprofEnable || config.Common.PProfEnable {
		pprof.Start()
	}

	args := clients.Args{
		ConnectionsPerCPU: connectionsPerCPU,
		ServersStr:        serversStr,
		Discovery:         discovery,
		UserName:          userName,
		Files:             files,
		TrustAllHosts:     trustAllHosts,
		PingTimeout:       pingTimeoutS,
		Regex:             regex,
		Mode:              omode.TailClient,
	}

	var client clients.Client
	var err error

	switch queryStr {
	case "":
		if client, err = clients.NewTailClient(args); err != nil {
			panic(err)
		}
	default:
		if client, err = clients.NewMaprClient(args, queryStr); err != nil {
			panic(err)
		}
	}

	client.Start()
}

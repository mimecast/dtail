package main

import (
	"context"
	"flag"
	"os"

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
	var connectionsPerCPU int
	var debugEnable bool
	var discovery string
	var displayVersion bool
	var files string
	var noColor bool
	var queryStr string
	var serversStr string
	var quietEnable bool
	var sshPort int
	var timeout int
	var trustAllHosts bool
	var privateKeyPathFile string

	userName := user.Name()

	flag.BoolVar(&debugEnable, "debug", false, "Activate debug messages")
	flag.BoolVar(&displayVersion, "version", false, "Display version")
	flag.BoolVar(&noColor, "noColor", false, "Disable ANSII terminal colors")
	flag.BoolVar(&quietEnable, "quiet", false, "Reduce output")
	flag.BoolVar(&trustAllHosts, "trustAllHosts", false, "Auto trust all unknown host keys")
	flag.IntVar(&connectionsPerCPU, "cpc", 10, "How many connections established per CPU core concurrently")
	flag.IntVar(&sshPort, "port", 2222, "SSH server port")
	flag.IntVar(&timeout, "timeout", 0, "Max time dtail server will collect data until disconnection")
	flag.StringVar(&cfgFile, "cfg", "", "Config file path")
	flag.StringVar(&discovery, "discovery", "", "Server discovery method")
	flag.StringVar(&files, "files", "", "File(s) to read")
	flag.StringVar(&queryStr, "query", "", "Map reduce query")
	flag.StringVar(&serversStr, "servers", "", "Remote servers to connect")
	flag.StringVar(&userName, "user", userName, "Your system user name")
	flag.StringVar(&privateKeyPathFile, "key", "", "Path to private key")

	flag.Parse()

	config.Read(cfgFile, sshPort)
	color.Colored = !noColor

	if displayVersion {
		version.PrintAndExit()
	}

	ctx := context.TODO()
	logger.Start(ctx, logger.Modes{Debug: debugEnable || config.Common.DebugEnable, Quiet: quietEnable})

	args := clients.Args{
		ConnectionsPerCPU:  connectionsPerCPU,
		ServersStr:         serversStr,
		Discovery:          discovery,
		UserName:           userName,
		What:               files,
		TrustAllHosts:      trustAllHosts,
		Mode:               omode.MapClient,
		Timeout:            timeout,
		PrivateKeyPathFile: privateKeyPathFile,
	}

	client, err := clients.NewMaprClient(args, queryStr)
	if err != nil {
		panic(err)
	}

	status := client.Start(ctx)
	logger.Flush()
	os.Exit(status)
}

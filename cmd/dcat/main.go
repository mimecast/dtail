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
	"github.com/mimecast/dtail/internal/user"
	"github.com/mimecast/dtail/internal/version"
)

// The evil begins here.
func main() {
	var args config.Args
	var displayVersion bool

	userName := user.Name()

	flag.BoolVar(&args.NoColor, "noColor", false, "Disable ANSII terminal colors")
	flag.BoolVar(&args.Quiet, "quiet", false, "Quiet output mode")
	flag.BoolVar(&args.Spartan, "spartan", false, "Spartan output mode")
	flag.BoolVar(&args.TrustAllHosts, "trustAllHosts", false, "Trust all unknown host keys")
	flag.BoolVar(&displayVersion, "version", false, "Display version")
	flag.IntVar(&args.ConnectionsPerCPU, "cpc", 10, "How many connections established per CPU core concurrently")
	flag.IntVar(&args.SSHPort, "port", 2222, "SSH server port")
	flag.StringVar(&args.ConfigFile, "cfg", "", "Config file path")
	flag.StringVar(&args.Discovery, "discovery", "", "Server discovery method")
	flag.StringVar(&args.LogLevel, "logLevel", "", "Log level")
	flag.StringVar(&args.PrivateKeyPathFile, "key", "", "Path to private key")
	flag.StringVar(&args.ServersStr, "servers", "", "Remote servers to connect")
	flag.StringVar(&args.UserName, "user", userName, "Your system user name")
	flag.StringVar(&args.What, "files", "", "File(s) to read")

	flag.Parse()
	config.Setup(&args, flag.Args())

	if displayVersion {
		version.PrintAndExit()
	}
	if !args.Spartan {
		version.Print()
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	dlog.Start(ctx, &wg, dlog.CLIENT, config.Common.LogLevel)

	client, err := clients.NewCatClient(args)
	if err != nil {
		panic(err)
	}

	status := client.Start(ctx, signal.InterruptCh(ctx))
	cancel()

	wg.Wait()
	os.Exit(status)
}

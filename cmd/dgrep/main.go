package main

import (
	"context"
	"flag"
	"os"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/io/signal"
	"github.com/mimecast/dtail/internal/user"
	"github.com/mimecast/dtail/internal/version"
)

// The evil begins here.
func main() {
	var args clients.Args
	var cfgFile string
	var debugEnable bool
	var displayVersion bool
	var grep string
	var noColor bool
	var sshPort int

	userName := user.Name()

	flag.BoolVar(&args.RegexInvert, "invert", false, "Invert regex")
	flag.BoolVar(&args.TrustAllHosts, "trustAllHosts", false, "Auto trust all unknown host keys")
	flag.BoolVar(&debugEnable, "debug", false, "Activate debug messages")
	flag.BoolVar(&displayVersion, "version", false, "Display version")
	flag.BoolVar(&noColor, "noColor", false, "Disable ANSII terminal colors")
	flag.BoolVar(&args.Quiet, "quiet", false, "Quiet output mode")
	flag.IntVar(&args.ConnectionsPerCPU, "cpc", 10, "How many connections established per CPU core concurrently")
	flag.IntVar(&sshPort, "port", 2222, "SSH server port")
	flag.StringVar(&args.Discovery, "discovery", "", "Server discovery method")
	flag.StringVar(&args.PrivateKeyPathFile, "key", "", "Path to private key")
	flag.StringVar(&args.ServersStr, "servers", "", "Remote servers to connect")
	flag.StringVar(&args.UserName, "user", userName, "Your system user name")
	flag.StringVar(&args.What, "files", "", "File(s) to read")
	flag.StringVar(&cfgFile, "cfg", "", "Config file path")

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

	ctx := context.TODO()
	logger.Start(ctx, logger.Modes{
		Debug: debugEnable || config.Common.DebugEnable,
		Quiet: args.Quiet,
	})

	client, err := clients.NewGrepClient(args)
	if err != nil {
		panic(err)
	}

	status := client.Start(ctx, signal.InterruptCh(ctx))
	logger.Flush()
	os.Exit(status)
}

package main

import (
	"context"
	"flag"
	"io/ioutil"
	"os"
	"strings"

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
	var background string
	var cfgFile string
	var command string
	var debugEnable bool
	var displayVersion bool
	var jobName string
	var noColor bool
	var sshPort int

	userName := user.Name()

	flag.BoolVar(&args.TrustAllHosts, "trustAllHosts", false, "Auto trust all unknown host keys")
	flag.BoolVar(&debugEnable, "debug", false, "Activate debug messages")
	flag.BoolVar(&displayVersion, "version", false, "Display version")
	flag.BoolVar(&noColor, "noColor", false, "Disable ANSII terminal colors")
	flag.IntVar(&args.ConnectionsPerCPU, "cpc", 10, "How many connections established per CPU core concurrently")
	flag.IntVar(&args.Timeout, "timeout", 0, "Command execution timeout")
	flag.IntVar(&sshPort, "port", 2222, "SSH server port")
	flag.StringVar(&args.Discovery, "discovery", "", "Server discovery method")
	flag.StringVar(&args.PrivateKeyPathFile, "key", "", "Path to private key")
	flag.StringVar(&args.ServersStr, "servers", "", "Remote servers to connect")
	flag.StringVar(&args.UserName, "user", userName, "Your system user name")
	flag.StringVar(&background, "background", "", "Can be one of 'start', 'cancel', 'list' or empty")
	flag.StringVar(&cfgFile, "cfg", "", "Config file path")
	flag.StringVar(&command, "command", "", "Command to run")
	flag.StringVar(&jobName, "name", "", "The job name (if run in background)")

	flag.Parse()

	config.Read(cfgFile, sshPort)
	color.Colored = !noColor

	if displayVersion {
		version.PrintAndExit()
	}

	ctx := context.TODO()
	logger.Start(ctx, logger.Modes{Debug: debugEnable || config.Common.DebugEnable})

	args.What, args.Arguments = readCommand(command)
	client, err := clients.NewRunClient(args, background, jobName)
	if err != nil {
		panic(err)
	}

	status := client.Start(ctx, signal.InterruptCh(ctx))
	logger.Flush()
	os.Exit(status)
}

func readCommand(command string) (string, []string) {
	splitted := strings.Split(command, " ")

	script := splitted[0]
	if _, err := os.Stat(script); os.IsNotExist(err) {
		var commandArgs []string
		return command, commandArgs
	}
	commandArgs := splitted[1:]

	bytes, err := ioutil.ReadFile(script)
	if err != nil {
		panic(err)
	}

	return string(bytes), commandArgs
}

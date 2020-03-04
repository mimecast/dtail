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
	"github.com/mimecast/dtail/internal/user"
	"github.com/mimecast/dtail/internal/version"
)

// The evil begins here.
func main() {
	var background string
	var cfgFile string
	var command string
	var connectionsPerCPU int
	var debugEnable bool
	var discovery string
	var displayVersion bool
	var jobName string
	var noColor bool
	var serversStr string
	var quietEnable bool
	var sshPort int
	var timeout int
	var trustAllHosts bool

	userName := user.Name()

	flag.BoolVar(&debugEnable, "debug", false, "Activate debug messages")
	flag.BoolVar(&displayVersion, "version", false, "Display version")
	flag.BoolVar(&noColor, "noColor", false, "Disable ANSII terminal colors")
	flag.BoolVar(&quietEnable, "quiet", false, "Reduce output")
	flag.BoolVar(&trustAllHosts, "trustAllHosts", false, "Auto trust all unknown host keys")
	flag.IntVar(&connectionsPerCPU, "cpc", 10, "How many connections established per CPU core concurrently")
	flag.IntVar(&sshPort, "port", 2222, "SSH server port")
	flag.IntVar(&timeout, "timeout", 0, "Command execution timeout")
	flag.StringVar(&background, "background", "", "Can be one of 'start', 'cancel', 'list' or empty")
	flag.StringVar(&cfgFile, "cfg", "", "Config file path")
	flag.StringVar(&command, "command", "", "Command to run")
	flag.StringVar(&discovery, "discovery", "", "Server discovery method")
	flag.StringVar(&jobName, "name", "", "The job name (if run in background)")
	flag.StringVar(&serversStr, "servers", "", "Remote servers to connect")
	flag.StringVar(&userName, "user", userName, "Your system user name")

	flag.Parse()

	config.Read(cfgFile, sshPort)
	color.Colored = !noColor

	if displayVersion {
		version.PrintAndExit()
	}

	ctx := context.TODO()
	logger.Start(ctx, logger.Modes{Debug: debugEnable, Quiet: quietEnable})

	command, commandArgs := readCommand(command)
	args := clients.Args{
		ConnectionsPerCPU: connectionsPerCPU,
		ServersStr:        serversStr,
		Discovery:         discovery,
		UserName:          userName,
		What:              command,
		Arguments:         commandArgs,
		TrustAllHosts:     trustAllHosts,
		Timeout:           timeout,
	}

	client, err := clients.NewRunClient(args, background, jobName)
	if err != nil {
		panic(err)
	}

	status := client.Start(ctx)
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

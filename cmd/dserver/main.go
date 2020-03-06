package main

import (
	"context"
	"flag"
	"os"
	"time"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/server"
	"github.com/mimecast/dtail/internal/user"
	"github.com/mimecast/dtail/internal/version"
)

// The evil begins here.
func main() {
	var cfgFile string
	var debugEnable bool
	var displayVersion bool
	var noColor bool
	var shutdownAfter int
	var sshPort int

	user.NoRootCheck()

	flag.BoolVar(&debugEnable, "debug", false, "Activate debug messages")
	flag.BoolVar(&displayVersion, "version", false, "Display version")
	flag.BoolVar(&noColor, "noColor", false, "Disable ANSII terminal colors")
	flag.IntVar(&shutdownAfter, "shutdownAfter", 0, "Automatically shutdown after so many seconds")
	flag.IntVar(&sshPort, "port", 2222, "SSH server port")
	flag.StringVar(&cfgFile, "cfg", "", "Config file path")

	flag.Parse()

	config.Read(cfgFile, sshPort)
	color.Colored = !noColor

	if displayVersion {
		version.PrintAndExit()
	}

	ctx := context.Background()

	serverEnable := true
	silentEnable := false
	nothingEnable := false
	logger.Start(ctx, serverEnable, debugEnable, silentEnable, nothingEnable)

	if shutdownAfter > 0 {
		go func() {
			defer os.Exit(1)

			logger.Info("Enabling auto shutdown timer", shutdownAfter)
			select {
			case <-time.After(time.Duration(shutdownAfter) * time.Second):
				logger.Info("Auto shutdown timer reached, shutting down now")
			case <-ctx.Done():
			}
		}()
	}

	serv := server.New()
	status := serv.Start(ctx)
	logger.Flush()
	os.Exit(status)
}

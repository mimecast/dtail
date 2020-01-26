package pprof

import (
	"fmt"
	"net/http"
	_ "net/http"
	_ "net/http/pprof"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/logger"
)

// Start the profiler HTTP server.
func Start() {
	bindAddr := fmt.Sprintf("%s:%d", config.Common.PProfBindAddress, config.Common.PProfPort)
	logger.Info("Starting PProf server", bindAddr)
	go http.ListenAndServe(bindAddr, nil)
}

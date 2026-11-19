package jobs

import (
	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/logging"
)

var jobTestLoggers = clients.NewLoggerDependencies(
	clientlog.NopLogger{}, logging.NopLogger{}, logging.NopLogger{},
)

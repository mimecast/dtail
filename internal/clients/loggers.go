package clients

import (
	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/logging"
)

// LoggerDependencies preserves the distinct logger roles used by a client
// process, including its in-process server in serverless mode.
type LoggerDependencies struct {
	Client clientlog.Logger
	Server logging.Logger
	Common logging.Logger
}

// NewLoggerDependencies builds the logger role bundle required by clients.
func NewLoggerDependencies(client clientlog.Logger, server, common logging.Logger) LoggerDependencies {
	return LoggerDependencies{Client: client, Server: server, Common: common}.normalized()
}

func (l LoggerDependencies) normalized() LoggerDependencies {
	l.Client = clientlog.OrNop(l.Client)
	l.Server = logging.OrNop(l.Server)
	l.Common = logging.OrNop(l.Common)
	return l
}

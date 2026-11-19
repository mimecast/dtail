package clients

import (
	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/clients/handlers"
	sessionspec "github.com/mimecast/dtail/internal/session"
)

type clientProfile struct {
	newHandler func(server string) handlers.Handler
	retry      bool
	commit     func(spec sessionspec.Spec, generation uint64) error
}

func clientHandlerProfile(logger clientlog.Logger, retry bool) clientProfile {
	return clientProfile{
		newHandler: func(server string) handlers.Handler {
			return handlers.NewClientHandler(server, logger)
		},
		retry: retry,
	}
}

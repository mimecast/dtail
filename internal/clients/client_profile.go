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
	// failedCommandStatus is the least exit status of a connection whose
	// server reported a failed command (see sessionStatus); 0 leaves the
	// handler's status as it is, so that dtail's follow mode, dmap (whose
	// scheduled modes handle the failures themselves and whose interactive
	// mode only warns, see MaprClient.finish) and dtailhealth keep their
	// exit statuses.
	failedCommandStatus int
	// cutShortStatus is the least exit status of a connection whose session
	// ended without the server's close handshake although the client did not
	// end it itself (see sessionStatus and exitStatusCutShort); 0 leaves the
	// handler's status as it is, so that dtail's follow mode (which
	// reconnects), dmap (whose scheduled modes check the outcome themselves
	// and whose interactive mode only warns, see MaprClient.finish) and
	// dtailhealth keep their exit statuses.
	cutShortStatus int
}

func clientHandlerProfile(logger clientlog.Logger, retry bool) clientProfile {
	return clientProfile{
		newHandler: func(server string) handlers.Handler {
			return handlers.NewClientHandler(server, logger)
		},
		retry: retry,
	}
}

// readClientProfile is the profile of the one-shot read clients dcat and
// dgrep: no retry, exit status ExitStatusFailedCommand when a server
// reported a failed command, e.g. a file it could not read, and
// exitStatusCutShort when a session was cut short, e.g. by a killed server.
func readClientProfile(logger clientlog.Logger) clientProfile {
	profile := clientHandlerProfile(logger, false)
	profile.failedCommandStatus = ExitStatusFailedCommand
	profile.cutShortStatus = exitStatusCutShort
	return profile
}

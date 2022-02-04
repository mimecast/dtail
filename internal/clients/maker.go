package clients

import (
	"github.com/mimecast/dtail/internal/clients/handlers"
)

// maker interface helps to re-use code in all DTail client implementations.
// All clients share the baseClient but have different connection handlers
// and send different commands to the DTail server.
type maker interface {
	makeHandler(server string) handlers.Handler
	makeCommands() (commands []string)
}

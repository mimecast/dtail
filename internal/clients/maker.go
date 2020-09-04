package clients

import (
	"github.com/mimecast/dtail/internal/clients/handlers"
)

type maker interface {
	makeHandler(server string) handlers.Handler
	makeCommands() (commands []string)
}

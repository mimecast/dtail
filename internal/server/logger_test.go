package server

import (
	"github.com/mimecast/dtail/internal/handlers"
	"github.com/mimecast/dtail/internal/logging"
)

var serverTestLoggers = handlers.HandlerLoggers{
	Diagnostics: logging.NopLogger{},
	Reader:      logging.NopLogger{},
}

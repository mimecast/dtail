package loggers

// don't log anything
type none struct{}

var _ Logger = none{}
var _ RawBytesWriter = none{}

func (none) Log(message string) {
	// This is empty because the none isn't logging but has to satisfy the interface.
}

func (none) LogWithColors(message, coloredMessage string) {
	// This is empty because the none isn't logging but has to satisfy the interface.
}

func (none) Raw(message string) {
	// This is empty because the none isn't logging but has to satisfy the interface.
}

func (none) RawBytes(message []byte) {
	// This is empty because the none isn't logging but has to satisfy the interface.
}

func (none) RawWithColors(message, coloredMessage string) {
	// This is empty because the none isn't logging but has to satisfy the interface.
}

func (none) Flush() {
	// This is empty because the none isn't logging but has to satisfy the interface.
}

func (none) SupportsColors() bool { return false }

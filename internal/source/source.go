package source

// Source specifies the origin of either the current process (dtail is a client
// process, dserver is a server process) or the source code package (e.g.
// dserver server side code or dtail client side code). Notice that dtail client
// may also executes server code directly (e.g. via serverless mode) and that
// the dserver may also executes client code (e.g. via scheduled server side
// mapreduce queries).
type Source int

const (
	// Client process or source code package.
	Client Source = iota
	// Server process or source code package.
	Server Source = iota
	// HealthCheck process or client source code package.
	HealthCheck Source = iota
)

func (s Source) String() string {
	switch s {
	case Client:
		return "CLIENT"
	case Server:
		return "SERVER"
	case HealthCheck:
		return "HEALTHCHECK"
	}
	panic("Unknown source type")
}

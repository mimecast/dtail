package logger

// Modes specifies the logging mode.
type Modes struct {
	Server      bool
	Trace       bool
	Debug       bool
	Quiet       bool
	Nothing     bool
	logToStdout bool
	logToFile   bool
}

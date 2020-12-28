package logger

// Modes specifies the logging mode.
type Modes struct {
	Server      bool
	Trace       bool
	Debug       bool
	Nothing     bool
	Quiet       bool
	logToStdout bool
	logToFile   bool
}

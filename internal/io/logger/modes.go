package logger

// Modes specifies the logging mode.
type Modes struct {
	Server      bool
	Trace       bool
	Debug       bool
	Nothing     bool
	logToStdout bool
	logToFile   bool
}

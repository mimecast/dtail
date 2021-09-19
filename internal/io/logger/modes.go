package logger

// Modes specifies the logging mode.
type Modes struct {
	Debug       bool
	logToFile   bool
	logToStdout bool
	Nothing     bool
	Quiet       bool
	Server      bool
	Trace       bool
	UnitTest    bool
}

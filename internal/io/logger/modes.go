package logger

type Modes struct {
	Server      bool
	Trace       bool
	Debug       bool
	Quiet       bool
	Nothing     bool
	logToStdout bool
	logToFile   bool
}

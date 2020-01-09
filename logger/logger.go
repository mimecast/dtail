package logger

import (
	"bufio"
	"dtail/color"
	"dtail/config"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	clientStr string = "CLIENT"
	serverStr string = "SERVER"
	infoStr   string = "INFO"
	warnStr   string = "WARN"
	errorStr  string = "ERROR"
	fatalStr  string = "FATAL"
	debugStr  string = "DEBUG"
	traceStr  string = "TRACE"
)

// Synchronise access to logging.
var mutex sync.Mutex

// File descriptor of log file when logToFile enabled.
var fd *os.File

// File write buffer of log file when logToFile enabled.
var writer *bufio.Writer

// File write buffer of stdout when logToStdout enabled.
var stdoutWriter *bufio.Writer

// Current hostname.
var hostname string

// Used to detect change of day (create one log file per day0
var lastDateStr string

// True if log in server mode, false if log in client mode.
var serverEnable bool

// Used to make logging non-blocking.
var logBufCh chan buf
var stdoutBufCh chan string

// Stdout channel, required to pause output
var pauseCh chan struct{}
var resumeCh chan struct{}

// Tell the logger that we are done, program shuts down
var stop chan struct{}
var stdoutFlushed chan struct{}

// Tell the logger about logrotation
var rotateCh chan os.Signal

// LogMode allows to specify the verbosity of logging.
type LogMode int

// Possible log modes.
const (
	NormalMode  LogMode = iota
	DebugMode   LogMode = iota
	SilentMode  LogMode = iota
	TraceMode   LogMode = iota
	NothingMode LogMode = iota
)

// Mode is the current log mode in use.
var Mode LogMode

// LogStrategy allows to specify a log rotation strategy.
type LogStrategy int

// Possible log strategies.
const (
	NormalStrategy LogStrategy = iota
	DailyStrategy  LogStrategy = iota
	StdoutStrategy LogStrategy = iota
)

// Strategy is the current log strattegy used.
var Strategy LogStrategy

// Enables logging to stdout.
var logToStdout bool

// Enables logging to file.
var logToFile bool

// Helper type to make logging non-blocking.
type buf struct {
	time    time.Time
	message string
}

// Init logging.
func Init(myServerEnable bool, mode LogMode, strategy LogStrategy) {
	stdoutWriter = bufio.NewWriter(os.Stdout)

	serverEnable = myServerEnable
	Mode = mode
	Strategy = strategy

	if Mode == NothingMode {
		return
	}

	switch Strategy {
	case DailyStrategy:
		_, err := os.Stat(config.Common.LogDir)
		logToFile = !os.IsNotExist(err)
		logToStdout = !serverEnable || Mode == DebugMode || Mode == TraceMode
	case StdoutStrategy:
		fallthrough
	default:
		logToFile = false
		logToStdout = true
	}

	fqdn, err := os.Hostname()
	if err != nil {
		panic(err)
	}
	s := strings.Split(fqdn, ".")
	hostname = s[0]

	pauseCh = make(chan struct{})
	resumeCh = make(chan struct{})
	stop = make(chan struct{})
	stdoutFlushed = make(chan struct{})

	// Setup logrotation
	rotateCh = make(chan os.Signal, 1)
	signal.Notify(rotateCh, syscall.SIGHUP)

	if logToStdout {
		stdoutBufCh = make(chan string, runtime.NumCPU()*100)
		go writeToStdout()
	}

	if logToFile {
		logBufCh = make(chan buf, runtime.NumCPU()*100)
		go writeToFile()
	}
}

// Info message logging.
func Info(args ...interface{}) string {
	if serverEnable {
		return log(serverStr, infoStr, args)
	}

	return log(clientStr, infoStr, args)
}

// Warn message logging.
func Warn(args ...interface{}) string {
	if serverEnable {
		return log(serverStr, warnStr, args)
	}

	return log(clientStr, warnStr, args)
}

// Error message logging.
func Error(args ...interface{}) string {
	if serverEnable {
		return log(serverStr, errorStr, args)
	}

	return log(clientStr, errorStr, args)
}

// FatalExit logs an error and exists the process.
func FatalExit(args ...interface{}) {
	what := clientStr
	if serverEnable {
		what = serverStr
	}
	log(what, fatalStr, args)

	time.Sleep(time.Second)
	mutex.Lock()
	defer mutex.Unlock()

	closeWriter()
	os.Exit(3)
}

// Debug message logging.
func Debug(args ...interface{}) string {
	if Mode == DebugMode || Mode == TraceMode {
		if serverEnable {
			return log(serverStr, debugStr, args)
		}
		return log(clientStr, debugStr, args)
	}

	return ""
}

// Trace message logging.
func Trace(args ...interface{}) string {
	if Mode == TraceMode {
		if serverEnable {
			return log(serverStr, traceStr, args)
		}
		return log(clientStr, traceStr, args)
	}

	return ""
}

// Write log line to buffer and/or log file.
func write(what, severity, message string) {
	if logToStdout && (Mode != SilentMode || severity != warnStr) {
		line := fmt.Sprintf("%s|%s|%s|%s\n", what, hostname, severity, message)

		if color.Colored {
			line = color.Colorfy(line)
		}

		stdoutBufCh <- line
	}

	if logToFile {
		t := time.Now()
		timeStr := t.Format("20060102-150405")
		logBufCh <- buf{
			time:    t,
			message: fmt.Sprintf("%s|%s|%s|%s\n", severity, timeStr, what, message),
		}
	}
}

// Generig log message.
func log(what string, severity string, args []interface{}) string {
	if Mode == NothingMode {
		return ""
	}

	var messages []string

	for _, arg := range args {
		switch v := arg.(type) {
		case string:
			messages = append(messages, v)
		case int:
			messages = append(messages, fmt.Sprintf("%d", v))
		case error:
			messages = append(messages, v.Error())
		default:
			messages = append(messages, fmt.Sprintf("%v", v))
		}
	}

	message := strings.Join(messages, "|")
	write(what, severity, message)

	return fmt.Sprintf("%s|%s", severity, message)
}

// Raw message logging.
func Raw(message string) {
	if Mode == NothingMode {
		return
	}

	if logToStdout {
		if color.Colored {
			message = color.Colorfy(message)
		}
		stdoutBufCh <- message
	}

	if logToFile {
		logBufCh <- buf{time.Now(), message}
	}
}

// Close log writer (e.g. on change of day).
func closeWriter() {
	if writer != nil {
		writer.Flush()
		fd.Close()
	}
}

// Return the correct log file writer
func fileWriter(dateStr string) *bufio.Writer {
	if dateStr != lastDateStr {
		return updateFileWriter(dateStr)
	}

	// Check for log rotation signal
	select {
	case <-rotateCh:
		stdoutWriter.WriteString("Received signal for logrotation\n")
		return updateFileWriter(dateStr)
	default:
	}

	return writer
}

// Update log file writer
func updateFileWriter(dateStr string) *bufio.Writer {
	// Detected change of day. Close current writer and create a new one.
	mutex.Lock()
	defer mutex.Unlock()
	closeWriter()

	if _, err := os.Stat(config.Common.LogDir); os.IsNotExist(err) {
		if err = os.MkdirAll(config.Common.LogDir, 0755); err != nil {
			panic(err)
		}
	}

	logFile := fmt.Sprintf("%s/%s.log", config.Common.LogDir, dateStr)
	newFd, err := os.OpenFile(logFile, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		panic(err)
	}

	fd = newFd
	writer = bufio.NewWriterSize(fd, 1)
	lastDateStr = dateStr

	return writer
}

func flushStdout() {
	defer close(stdoutFlushed)

	for {
		select {
		case message := <-stdoutBufCh:
			stdoutWriter.WriteString(message)
		default:
			stdoutWriter.Flush()
			return
		}
	}
}

func writeToStdout() {
	for {
		select {
		case message := <-stdoutBufCh:
			stdoutWriter.WriteString(message)
		case <-time.After(time.Millisecond * 100):
			stdoutWriter.Flush()
		case <-pauseCh:
		PAUSE:
			for {
				select {
				case <-stdoutBufCh:
				case <-resumeCh:
					break PAUSE
				case <-stop:
					return
				}
			}
		case <-stop:
			flushStdout()
			return
		}
	}
}

func writeToFile() {
	for {
		select {
		case buf := <-logBufCh:
			dateStr := buf.time.Format("20060102")
			w := fileWriter(dateStr)
			w.WriteString(buf.message)
		case <-pauseCh:
		PAUSE:
			for {
				select {
				case <-stdoutBufCh:
				case <-resumeCh:
					break PAUSE
				case <-stop:
					return
				}
			}
		case <-stop:
			return
		}
	}
}

// Pause logging.
func Pause() {
	if logToStdout {
		pauseCh <- struct{}{}
	}
	if logToFile {
		pauseCh <- struct{}{}
	}
}

// Resume logging (after pausing).
func Resume() {
	if logToStdout {
		resumeCh <- struct{}{}
	}
	if logToFile {
		resumeCh <- struct{}{}
	}
}

// Stop logging.
func Stop() {
	close(stop)
	<-stdoutFlushed
}

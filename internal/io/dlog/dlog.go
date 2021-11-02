package dlog

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog/loggers"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/source"
)

// Client is the log handler for the client packages.
var Client *DLog

// Server is the log handler for the server packages.
var Server *DLog

// Common is the log handler for all other packages.
var Common *DLog

var mutex sync.Mutex
var started bool

// Start logger(s).
func Start(ctx context.Context, wg *sync.WaitGroup, sourceProcess source.Source) {
	mutex.Lock()
	defer mutex.Unlock()

	if started {
		Common.FatalPanic("Logger already started")
	}

	Client = new(sourceProcess, source.Client)
	Server = new(sourceProcess, source.Server)
	Common = Client
	if sourceProcess == source.Server {
		Common = Server
	}

	var wg2 sync.WaitGroup
	wg2.Add(2)
	go Client.start(ctx, &wg2)
	go Server.start(ctx, &wg2)

	go rotation(ctx)
	go func() {
		wg2.Wait()
		wg.Done()
	}()

	started = true
}

// DLog is the DTail logger.
type DLog struct {
	logger loggers.Logger
	// Is this a DTail server or client process logging?
	sourceProcess source.Source
	// Is this a DTail server or client package logging? In serverless mode
	// the client can also execute code from the server package.
	sourcePackage source.Source
	// Max log level to log.
	maxLevel level
	// Current hostname.
	hostname string
}

// new creates a new DTail logger.
func new(sourceProcess, sourcePackage source.Source) *DLog {
	hostname, err := config.Hostname()
	if err != nil {
		panic(err)
	}
	logRotation := loggers.NewStrategy(config.Common.LogRotation)
	loggerName := config.Common.Logger
	level := newLevel(config.Common.LogLevel)

	return &DLog{
		logger:        loggers.Factory(sourceProcess.String(), loggerName, logRotation),
		sourceProcess: sourceProcess,
		sourcePackage: sourcePackage,
		maxLevel:      level,
		hostname:      hostname,
	}
}

func (d *DLog) start(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	var wg2 sync.WaitGroup
	wg2.Add(1)
	d.logger.Start(ctx, &wg2)
	<-ctx.Done()
	wg2.Wait()
}

func (d *DLog) log(level level, args []interface{}) string {
	if d.maxLevel < level {
		return ""
	}
	sb := pool.BuilderBuffer.Get().(*strings.Builder)
	defer pool.RecycleBuilderBuffer(sb)
	now := time.Now()

	switch d.sourceProcess {
	case source.Client:
		sb.WriteString(d.sourcePackage.String())
		sb.WriteString(protocol.FieldDelimiter)
		sb.WriteString(d.hostname)
		sb.WriteString(protocol.FieldDelimiter)
		sb.WriteString(level.String())
	default:
		sb.WriteString(level.String())
		sb.WriteString(protocol.FieldDelimiter)
		sb.WriteString(now.Format("20060102-150405"))
	}
	sb.WriteString(protocol.FieldDelimiter)
	d.writeArgStrings(sb, args)

	message := sb.String()
	if !config.Client.TermColorsEnable || !d.logger.SupportsColors() {
		d.logger.Log(now, message)
		return message
	}

	d.logger.LogWithColors(now, message, brush.Colorfy(message))
	return message
}

func (d *DLog) writeArgStrings(sb *strings.Builder, args []interface{}) {
	for i, arg := range args {
		if i > 0 {
			sb.WriteString(protocol.FieldDelimiter)
		}
		switch v := arg.(type) {
		case string:
			sb.WriteString(v)
		case error:
			sb.WriteString(v.Error())
		default:
			sb.WriteString(fmt.Sprintf("%v", v))
		}
	}
}

// FatalPanic terminates the process with a fatal error.
func (d *DLog) FatalPanic(args ...interface{}) {
	d.log(Fatal, args)
	d.Flush()

	var sb strings.Builder
	d.writeArgStrings(&sb, args)
	panic(sb.String())
}

// Fatal logs a fatal error.
func (d *DLog) Fatal(args ...interface{}) string {
	return d.log(Fatal, args)
}

// Error logging.
func (d *DLog) Error(args ...interface{}) string {
	return d.log(Error, args)
}

// Warn logs a warning message.
func (d *DLog) Warn(args ...interface{}) string {
	return d.log(Warn, args)
}

// Info logging.
func (d *DLog) Info(args ...interface{}) string {
	return d.log(Info, args)
}

// Verbose logging.
func (d *DLog) Verbose(args ...interface{}) string {
	return d.log(Verbose, args)
}

// Debug logging.
func (d *DLog) Debug(args ...interface{}) string {
	return d.log(Debug, args)
}

// Trace logging.
func (d *DLog) Trace(args ...interface{}) string {
	_, file, line, _ := runtime.Caller(1)
	args = append(args, fmt.Sprintf("at %s:%d", file, line))
	return d.log(Trace, args)
}

// Devel used for development purpose only logging (e.g. "print" debugging).
func (d *DLog) Devel(args ...interface{}) string {
	_, file, line, _ := runtime.Caller(1)
	args = append(args, fmt.Sprintf("at %s:%d", file, line))
	return d.log(Devel, args)
}

// Raw message logging.
func (d *DLog) Raw(message string) string {
	if !config.Client.TermColorsEnable || !d.logger.SupportsColors() {
		d.logger.Raw(time.Now(), message)
		return message
	}
	d.logger.RawWithColors(time.Now(), message, brush.Colorfy(message))
	return message
}

// Mapreduce logging.
func (d *DLog) Mapreduce(table string, data map[string]interface{}) string {
	args := make([]interface{}, len(data)+1)

	if d.sourceProcess == source.Server {
		// level|date-time|process|caller|cpus|goroutines|cgocalls|loadavg|uptime|MAPREDUCE:TABLE|key=value|...

		var loadAvg string
		if loadAvgBytes, err := ioutil.ReadFile("/proc/loadavg"); err == nil {
			tmp := string(loadAvgBytes)
			s := strings.SplitN(tmp, " ", 2)
			loadAvg = s[0]
		}

		var uptime string
		if uptimeBytes, err := ioutil.ReadFile("/proc/uptime"); err == nil {
			tmp := string(uptimeBytes)
			s := strings.SplitN(tmp, ".", 2)
			i, _ := strconv.ParseInt(s[0], 10, 64)
			t := time.Duration(i) * time.Second
			uptime = fmt.Sprintf("%v", t)
		}

		_, file, line, _ := runtime.Caller(1)
		args[0] = fmt.Sprintf("%d|%s:%d|%d|%d|%d|%s|%s|MAPREDUCE:%s",
			os.Getpid(),
			filepath.Base(file), line,
			runtime.NumCPU(),
			runtime.NumGoroutine(),
			runtime.NumCgoCall(),
			loadAvg,
			uptime,
			strings.ToUpper(table))
	} else {
		args[0] = fmt.Sprintf("STATS:%s", strings.ToUpper(table))
	}

	i := 1
	for k, v := range data {
		args[i] = fmt.Sprintf("%s=%v", k, v)
		i++
	}
	return d.log(Info, args)
}

// Flush the log buffers.
func (d *DLog) Flush() { d.logger.Flush() }

// Pause the logging.
func (d *DLog) Pause() { d.logger.Pause() }

// Resume the logging.
func (d *DLog) Resume() { d.logger.Resume() }

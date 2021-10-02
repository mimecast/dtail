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
func Start(ctx context.Context, wg *sync.WaitGroup, sourceProcess source, logLevel string) {
	mutex.Lock()
	defer mutex.Unlock()

	if started {
		Common.FatalPanic("Logger already started")
	}

	strategy := loggers.GetStrategy(config.Common.LogStrategy)
	level := newLevel(logLevel)

	switch sourceProcess {
	case CLIENT:
		// This is a DTail client process running.
		impl := loggers.FOUT
		Client = New(CLIENT, CLIENT, level, impl, strategy)
		Server = New(CLIENT, SERVER, level, impl, strategy)
		Common = Client
	case SERVER:
		// This is a DTail server process running.
		impl := loggers.FILE
		Client = New(SERVER, CLIENT, level, impl, strategy)
		Server = New(SERVER, SERVER, level, impl, strategy)
		Common = Server
	}

	var wg2 sync.WaitGroup
	wg2.Add(2)
	Client.start(ctx, &wg2)
	Server.start(ctx, &wg2)
	started = true

	go rotation(ctx)
	go func() {
		wg2.Wait()
		wg.Done()
	}()
}

// DLog is the DTail logger.
type DLog struct {
	logger loggers.Logger
	// Is this a DTail server or client process logging?
	sourceProcess source
	// Is this a DTail server or client package logging? In serverless mode
	// the client can also execute code from the server package.
	sourcePackage source
	// Max log level to log.
	maxLevel level
	// Current hostname.
	hostname string
}

// New creates a new DTail logger.
func New(sourceProcess, sourcePackage source, maxLevel level, impl loggers.Impl, strategy loggers.Strategy) *DLog {
	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}
	return &DLog{
		logger:        loggers.Factory(sourceProcess.String(), impl, strategy),
		sourceProcess: sourceProcess,
		sourcePackage: sourcePackage,
		maxLevel:      maxLevel,
		hostname:      hostname,
	}
}

func (d *DLog) start(ctx context.Context, wg *sync.WaitGroup) {
	go func() {
		defer wg.Done()
		var wg2 sync.WaitGroup
		wg2.Add(1)
		d.logger.Start(ctx, &wg2)
		<-ctx.Done()
		wg2.Wait()
	}()
}

func (d *DLog) log(level level, args []interface{}) string {
	if d.maxLevel < level {
		return ""
	}
	sb := pool.BuilderBuffer.Get().(*strings.Builder)
	defer pool.RecycleBuilderBuffer(sb)
	now := time.Now()

	switch d.sourceProcess {
	case CLIENT:
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

func (d *DLog) FatalPanic(args ...interface{}) {
	d.log(FATAL, args)
	d.logger.Flush()
	panic("Not recovering from this fatal error...")
}

func (d *DLog) Fatal(args ...interface{}) string {
	return d.log(FATAL, args)
}

func (d *DLog) Error(args ...interface{}) string {
	return d.log(ERROR, args)
}

func (d *DLog) Warn(args ...interface{}) string {
	return d.log(WARN, args)
}

func (d *DLog) Info(args ...interface{}) string {
	if d.sourcePackage == SERVER && d.sourceProcess != CLIENT {
		// This can be dtail client in serverless mode. In this case log all
		// info server messages as verbose.
		return d.log(VERBOSE, args)
	}
	return d.log(INFO, args)
}

func (d *DLog) Verbose(args ...interface{}) string {
	return d.log(VERBOSE, args)
}

func (d *DLog) Debug(args ...interface{}) string {
	return d.log(DEBUG, args)
}

func (d *DLog) Trace(args ...interface{}) string {
	return d.log(TRACE, args)
}

func (d *DLog) Devel(args ...interface{}) string {
	return d.log(DEVEL, args)
}

func (d *DLog) Raw(message string) string {
	if !config.Client.TermColorsEnable || !d.logger.SupportsColors() {
		d.logger.Log(time.Now(), message)
		return message
	}
	d.logger.LogWithColors(time.Now(), message, brush.Colorfy(message))
	return message
}

func (d *DLog) Mapreduce(table string, data map[string]interface{}) string {
	args := make([]interface{}, len(data)+1)

	if d.sourceProcess == SERVER {
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
	return d.log(INFO, args)
}

func (d *DLog) Flush()  { d.logger.Flush() }
func (d *DLog) Pause()  { d.logger.Pause() }
func (d *DLog) Resume() { d.logger.Resume() }

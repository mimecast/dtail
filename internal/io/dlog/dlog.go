package dlog

import (
	"context"
	"fmt"
	"os"
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

	level := newLevel(logLevel)
	switch sourceProcess {
	case CLIENT:
		// This is a DTail client process running.
		impl := loggers.FOUT
		Client = New(CLIENT, CLIENT, impl, level)
		Server = New(CLIENT, SERVER, impl, level)
		Common = Client
	case SERVER:
		// This is a DTail server process running.
		impl := loggers.FILE
		Client = New(SERVER, CLIENT, impl, level)
		Server = New(SERVER, SERVER, impl, level)
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
func New(sourceProcess, sourcePackage source, impl loggers.Impl, maxLevel level) *DLog {
	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}
	return &DLog{
		logger:        loggers.Factory(sourceProcess.String(), impl),
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

	// TODO: mC compatible SERVER mapreduce fields, no MAPREDUCE keyword in CLIENT mode
	args[0] = fmt.Sprintf("%s:%s", "MAPREDUCE", strings.ToUpper(table))

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

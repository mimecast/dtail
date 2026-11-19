package dlog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog/loggers"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/logging"
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

// DLog is the DTail logger.
type DLog struct {
	logger        loggers.Logger
	colorizer     Colorizer
	colorsEnabled bool
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

// Colorizer renders a message for a terminal color sink.
type Colorizer interface {
	Colorfy(string) string
}

var _ logging.Logger = (*DLog)(nil)

// newDLog creates a new DTail logger.
func newDLog(sourceProcess, sourcePackage source.Source, cfg config.RuntimeConfig,
	colorizer Colorizer) (*DLog, error) {
	if cfg.Common == nil {
		return nil, fmt.Errorf("logger configuration is unavailable")
	}
	hostname, err := cfg.Hostname()
	if err != nil {
		return nil, fmt.Errorf("resolve logger hostname: %w", err)
	}
	logRotation := loggers.NewStrategy(cfg.Common.LogRotation)
	loggerName := cfg.Common.Logger
	maxLevel, err := newLevel(cfg.Common.LogLevel)
	if err != nil {
		return nil, err
	}
	options := loggers.Options{LogDir: cfg.Common.LogDir}
	if cfg.Client != nil {
		options.LogPayload = cfg.Client.LogPayload
	}
	logger, err := loggers.Factory(sourceProcess.String(), loggerName, logRotation, options)
	if err != nil {
		return nil, err
	}
	colorsEnabled := cfg.Client != nil && cfg.Client.TermColorsEnable

	return &DLog{
		logger:        logger,
		colorizer:     colorizer,
		colorsEnabled: colorsEnabled,
		sourceProcess: sourceProcess,
		sourcePackage: sourcePackage,
		maxLevel:      maxLevel,
		hostname:      hostname,
	}, nil
}

// Start logger(s).
func Start(ctx context.Context, wg *sync.WaitGroup, sourceProcess source.Source,
	cfg config.RuntimeConfig, colorizer Colorizer) error {
	mutex.Lock()
	defer mutex.Unlock()

	if started {
		Common.FatalPanic("Logger already started")
	}

	clientLogger, err := newDLog(sourceProcess, source.Client, cfg, colorizer)
	if err != nil {
		return fmt.Errorf("create client logger: %w", err)
	}
	serverLogger, err := newDLog(sourceProcess, source.Server, cfg, colorizer)
	if err != nil {
		return fmt.Errorf("create server logger: %w", err)
	}
	Client = clientLogger
	Server = serverLogger
	Common = Client
	if sourceProcess == source.Server {
		Common = Server
	}

	var wg2 sync.WaitGroup
	wg2.Add(2)
	go Client.start(ctx, &wg2)
	go Server.start(ctx, &wg2)
	startRotation(ctx, &wg2, sourceProcess, loggers.FactoryRotate)

	go func() {
		wg2.Wait()
		wg.Done()
	}()

	started = true
	return nil
}

func (d *DLog) start(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	var wg2 sync.WaitGroup
	if starter, ok := d.logger.(loggers.Starter); ok {
		wg2.Add(1)
		starter.Start(ctx, &wg2)
	}
	<-ctx.Done()
	wg2.Wait()
}

// FatalPanic terminates the process with a fatal error.
func (d *DLog) FatalPanic(args ...any) {
	d.log(Fatal, args)
	d.Flush()

	var message strings.Builder
	protocol.AppendDiagnosticDetails(&message, args)
	panic(message.String())
}

// Fatal logs a fatal error.
func (d *DLog) Fatal(args ...any) string {
	return d.log(Fatal, args)
}

// Error logging.
func (d *DLog) Error(args ...any) string {
	return d.log(Error, args)
}

// Warn logs a warning message.
func (d *DLog) Warn(args ...any) string {
	return d.log(Warn, args)
}

// Info logging.
func (d *DLog) Info(args ...any) string {
	return d.log(Info, args)
}

// Verbose logging.
func (d *DLog) Verbose(args ...any) string {
	return d.log(Verbose, args)
}

// Debug logging.
func (d *DLog) Debug(args ...any) string {
	return d.log(Debug, args)
}

// TraceEnabled reports whether trace-level logging is currently active.
//
// It performs exactly the same maxLevel comparison as Trace's internal
// early-return (see below), letting callers on per-line hot paths gate the
// whole trace call — the variadic []any slice allocation plus the
// interface boxing of every non-pointer argument (uint64 line counts via
// runtime.convT64, strings via convTstring) — behind one cheap, inlinable,
// allocation-free branch. Without this guard those args are boxed at the call
// site before Trace even runs, so Trace's own early-return cannot save them.
//
// The receiver is nil-safe so call sites need no separate nil check on the
// package-level loggers (Server/Client/Common), which stay nil until Start.
// maxLevel is fixed at logger construction from RuntimeConfig.Common.LogLevel, so the
// result mirrors whatever level Trace itself would observe.
func (d *DLog) TraceEnabled() bool {
	return d != nil && d.maxLevel >= Trace
}

// Trace logging.
func (d *DLog) Trace(args ...any) string {
	// Early check to avoid expensive runtime.Caller when trace is disabled
	// This is a critical performance optimization for hot paths. Note that on
	// per-line hot paths callers should additionally gate with TraceEnabled()
	// so the argument boxing never happens; this check only saves runtime.Caller
	// and the log formatting, not the caller-side boxing of args.
	if d.maxLevel < Trace {
		return ""
	}
	_, file, line, _ := runtime.Caller(1)
	args = append(args, fmt.Sprintf("at %s:%d", file, line))
	return d.log(Trace, args)
}

// Devel used for development purpose only logging (e.g. "print" debugging).
func (d *DLog) Devel(args ...any) string {
	// Early check to avoid expensive runtime.Caller when devel is disabled
	if d.maxLevel < Devel {
		return ""
	}
	_, file, line, _ := runtime.Caller(1)
	args = append(args, fmt.Sprintf("at %s:%d", file, line))
	return d.log(Devel, args)
}

// Raw message logging.
func (d *DLog) Raw(message string) string {
	if !d.shouldColorize() {
		d.logger.Raw(message)
		return message
	}
	d.logger.RawWithColors(message, d.colorizer.Colorfy(message))
	return message
}

// RawBytes writes retrieved payload from a byte slice. Without colors it reaches
// a sink implementing loggers.RawBytesWriter with no string conversion; other
// sinks and the colorizer receive a string copy. The output is identical to
// Raw(string(message)). message is not retained.
func (d *DLog) RawBytes(message []byte) {
	if d.shouldColorize() {
		d.Raw(string(message))
		return
	}
	loggers.WriteRawBytes(d.logger, message)
}

// payloadFileTeer is the optional capability of a logger that can tee retrieved
// payload into its FILE sink without also writing it to stdout. Only the default
// fout logger implements it; stdout/none loggers have no file sink and are
// skipped via the type assertion in RawPayloadFileTee.
type payloadFileTeer interface {
	RawFileOnly(message string)
}

type payloadFileBytesTeer interface {
	RawFileOnlyBytes(message []byte)
}

// RawPayloadFileTee writes retrieved payload to the logger's FILE sink only
// (never stdout), honoring the client's --log-payload / Client.LogPayload
// opt-in. It is used by the serverless direct-output path, which emits payload
// straight to stdout and therefore bypasses the fout logger's own Raw tee. When
// the active logger has no file sink (stdout/none) or payload teeing is
// disabled, this is a no-op. stdout output is unaffected: the caller writes the
// same payload bytes to stdout itself, and this method only adds the file tee.
func (d *DLog) RawPayloadFileTee(message string) {
	if teer, ok := d.logger.(payloadFileTeer); ok {
		teer.RawFileOnly(message)
	}
}

// RawPayloadFileTeeBytes is the borrowed-byte form of RawPayloadFileTee.
// It writes only to an opt-in file tee, never stdout or the colorizer. The
// sink must consume or copy message before returning; string-only sinks get
// an owned copy, and sinks without a file tee remain a no-op.
func (d *DLog) RawPayloadFileTeeBytes(message []byte) {
	if teer, ok := d.logger.(payloadFileBytesTeer); ok {
		teer.RawFileOnlyBytes(message)
	} else if teer, ok := d.logger.(payloadFileTeer); ok {
		teer.RawFileOnly(string(message))
	}
}

// RawLog writes a pre-formatted message through the DIAGNOSTIC (Log) sink, so it
// reaches both stdout and — in the default fout logger — the daily log file.
//
// It differs from Raw, which uses the PAYLOAD sink: with the default
// Client.LogPayload=false, Raw is gated out of the file (bulk dcat/dgrep/dtail
// output must not grow the log). RawLog is for audit-worthy, already-formatted
// lines such as server-error reports, which must always be kept in the file like
// other diagnostics rather than being treated as bulk payload. The message is
// written verbatim (no level/hostname prefix); callers pre-format it and must
// NOT append a trailing newline, since the Log sink appends one.
func (d *DLog) RawLog(message string) string {
	if !d.shouldColorize() {
		d.logger.Log(message)
		return message
	}
	d.logger.LogWithColors(message, d.colorizer.Colorfy(message))
	return message
}

// Mapreduce logging.
func (d *DLog) Mapreduce(table string, data map[string]any) string {
	args := make([]any, len(data)+1)

	if d.sourceProcess == source.Server {
		// level|date-time|process|caller|cpus|goroutines|cgocalls|loadavg|uptime|MAPREDUCE:TABLE|key=value|...

		var loadAvg string
		if loadAvgBytes, err := os.ReadFile("/proc/loadavg"); err == nil {
			tmp := string(loadAvgBytes)
			s := strings.SplitN(tmp, " ", 2)
			loadAvg = s[0]
		}

		var uptime string
		if uptimeBytes, err := os.ReadFile("/proc/uptime"); err == nil {
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

// Pause the logging when the configured sink writes to an interactive terminal.
func (d *DLog) Pause() {
	if pauser, ok := d.logger.(loggers.Pauser); ok {
		pauser.Pause()
	}
}

// Resume the logging when the configured sink writes to an interactive terminal.
func (d *DLog) Resume() {
	if pauser, ok := d.logger.(loggers.Pauser); ok {
		pauser.Resume()
	}
}

func (d *DLog) log(level level, args []any) string {
	if d.maxLevel < level {
		return ""
	}
	encoded := pool.BuilderBuffer.Get().(*strings.Builder)
	defer pool.RecycleBuilderBuffer(encoded)

	switch d.sourceProcess {
	case source.Client:
		protocol.AppendDiagnostic(encoded, protocol.Diagnostic{
			Source:   d.sourcePackage.String(),
			Hostname: d.hostname,
			Level:    level.String(),
			Details:  args,
		})
	default:
		// The timestamp is read here, but a daily file sink picks its file
		// from a day name cached on its idle-flush tick, not from this time.
		// Around midnight a line stamped on one day can therefore land in the
		// other day's file. Accepted trade-off: the Logger contract carries no
		// per-line time so payload lines never read the clock.
		protocol.AppendTimedDiagnostic(encoded, protocol.TimedDiagnostic{
			Level:     level.String(),
			Timestamp: time.Now().Format("0102-150405"),
			Details:   args,
		})
	}
	message := encoded.String()

	if !d.shouldColorize() {
		d.logger.Log(message)
		return message
	}

	d.logger.LogWithColors(message, d.colorizer.Colorfy(message))
	return message
}

func (d *DLog) shouldColorize() bool {
	return d.colorsEnabled && d.colorizer != nil && d.logger.SupportsColors()
}

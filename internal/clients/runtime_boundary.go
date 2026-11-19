package clients

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	sessionHandlers "github.com/mimecast/dtail/internal/handlers"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	user "github.com/mimecast/dtail/internal/sessionuser"
	"golang.org/x/term"
)

type clientRuntimeBoundary struct {
	sshPort           int
	sshConnectTimeout time.Duration
	// sshHandshakeTimeout bounds the SSH handshake (Common.SSHHandshakeTimeoutMs).
	sshHandshakeTimeout time.Duration
	interruptPause      time.Duration
	serverCfg           *config.ServerConfig
	clientCfg           *config.ClientConfig
	output              *clientOutputFormatter
	loggers             LoggerDependencies
	stdout              func() io.Writer
	colorizer           *brush.Brush
	hostname            string
	hostnameErr         error
	// outputGate pauses the serverless direct output together with the client
	// logger while an interactive session's output is paused (Enter on the
	// control TTY, see interactivePause).
	outputGate *outputGate
}

func newClientRuntimeBoundary(cfg config.RuntimeConfig, loggers LoggerDependencies,
	colorizers ...*brush.Brush) *clientRuntimeBoundary {
	colorizer := firstColorizer(colorizers)
	sshPort := 2222
	sshConnectTimeout := 2 * time.Second
	sshHandshakeTimeout := 30 * time.Second
	if cfg.Common != nil {
		if cfg.Common.SSHPort > 0 {
			sshPort = cfg.Common.SSHPort
		}
		if cfg.Common.SSHConnectTimeoutMs > 0 {
			sshConnectTimeout = time.Duration(cfg.Common.SSHConnectTimeoutMs) * time.Millisecond
		}
		if cfg.Common.SSHHandshakeTimeoutMs > 0 {
			sshHandshakeTimeout = time.Duration(cfg.Common.SSHHandshakeTimeoutMs) * time.Millisecond
		}
	}

	loggers = loggers.normalized()
	hostname, hostnameErr := cfg.Hostname()
	return &clientRuntimeBoundary{
		sshPort:             sshPort,
		sshConnectTimeout:   sshConnectTimeout,
		sshHandshakeTimeout: sshHandshakeTimeout,
		interruptPause:      time.Second * time.Duration(config.InterruptTimeoutS),
		serverCfg:           cfg.Server,
		clientCfg:           cfg.Client,
		output:              newClientOutputFormatter(cfg.Client, rawQueryTokenColors(cfg, stdoutIsTerminal())),
		loggers:             loggers,
		stdout:              func() io.Writer { return os.Stdout },
		colorizer:           colorizer,
		hostname:            hostname,
		hostnameErr:         hostnameErr,
		outputGate:          newOutputGate(),
	}
}

func (r *clientRuntimeBoundary) SSHPort() int {
	return r.sshPort
}

func (r *clientRuntimeBoundary) SSHConnectTimeout() time.Duration {
	return r.sshConnectTimeout
}

func (r *clientRuntimeBoundary) SSHHandshakeTimeout() time.Duration {
	return r.sshHandshakeTimeout
}

func (r *clientRuntimeBoundary) InterruptPause() time.Duration {
	if r == nil || r.interruptPause <= 0 {
		return time.Second * time.Duration(config.InterruptTimeoutS)
	}
	return r.interruptPause
}

func (r *clientRuntimeBoundary) NewServerlessHandler(ctx context.Context, userName string) (sessionHandlers.Handler, error) {
	if r.hostnameErr != nil {
		return nil, fmt.Errorf("create serverless handler: resolve hostname: %w", r.hostnameErr)
	}
	var permissionLookup user.PermissionLookup
	if r.serverCfg != nil {
		permissionLookup = r.serverCfg.UserPermissions
	}

	serverUser, err := user.New(userName, "local(serverless)", permissionLookup, r.loggers.Server)
	if err != nil {
		return nil, err
	}

	return sessionHandlers.NewForUser(ctx, serverUser, r.serverlessDependencies())
}

// serverlessDependencies returns the process-owned resources of a serverless
// session handler, including its payload output and the colorizer that
// carries the client's terminal color decision (serverlessColorizer).
func (r *clientRuntimeBoundary) serverlessDependencies() sessionHandlers.Dependencies {
	dependencies := sessionHandlers.Dependencies{
		ServerConfig: r.serverCfg,
		Loggers: sessionHandlers.HandlerLoggers{
			Diagnostics: r.loggers.Server,
			Reader:      r.loggers.Common,
		},
		Capabilities: sessionHandlers.DetectCapabilities(),
		Colorizer:    r.serverlessColorizer(),
		Hostname:     r.hostname,
	}
	if r.serverCfg != nil {
		dependencies.CatLimiter = make(chan struct{}, positiveOrDefault(r.serverCfg.MaxConcurrentCats, 2))
		dependencies.TailLimiter = make(chan struct{}, positiveOrDefault(r.serverCfg.MaxConcurrentTails, 50))
		dependencies.AuthKeyStore = authkey.New(
			time.Duration(r.serverCfg.AuthKeyTTLSeconds)*time.Second,
			r.serverCfg.AuthKeyMaxPerUser,
		)
		dependencies.ServerlessOutput = r.serverlessOutputWriter()
	}
	return dependencies
}

// serverlessColorizer returns the colorizer of the serverless payload lines,
// which the in-process handler writes straight to stdout and so past the
// client logger and its color switch. With colors off (Client.TermColorsEnable
// false, as --noColor and --plain set it, or no client config) it is
// uncoloredTerminal, so the lines read as those of a dserver printed with
// --noColor. A nil brush yields a nil interface, never a typed nil pointer,
// and the handler then falls back to its default terminal colors.
func (r *clientRuntimeBoundary) serverlessColorizer() sessionHandlers.Colorizer {
	if r.clientCfg == nil || !r.clientCfg.TermColorsEnable {
		return uncoloredTerminal{}
	}
	if r.colorizer == nil {
		return nil
	}
	return r.colorizer
}

// uncoloredTerminal is the serverless colorizer with terminal colors off: it
// returns each line unchanged.
type uncoloredTerminal struct{}

func (uncoloredTerminal) Colorfy(line string) string {
	return line
}

func (r *clientRuntimeBoundary) serverlessOutputWriter() io.Writer {
	var output io.Writer = os.Stdout
	if r.stdout != nil {
		if configured := r.stdout(); configured != nil {
			output = configured
		}
	}
	// The gate wraps the whole output, file tee included, so a paused
	// interactive session holds back the payload as the paused client logger
	// does.
	if r.clientCfg == nil || !r.clientCfg.LogPayload {
		return r.outputGate.writer(output)
	}
	return r.outputGate.writer(io.MultiWriter(output, payloadFileTeeWriter{logger: r.loggers.Client}))
}

type payloadFileTeeWriter struct {
	logger logging.Logger
}

func (w payloadFileTeeWriter) Write(p []byte) (int, error) {
	clientlog.TeePayloadBytesToFile(w.logger, p)
	return len(p), nil
}

func positiveOrDefault(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

type interruptMessageFormatter interface {
	FormatInterruptMessage(index int, message string) string
}

// stdoutIsTerminal reports whether the process's stdout is a terminal.
func stdoutIsTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// rawQueryTokenColors reports whether the raw query above MapReduce result
// tables is highlighted word by word (PaintMaprRawQuery): only when stdout
// is a terminal and the client logger writes no payload into a log file
// (the "file" logger always does, "fout" with Client.LogPayload). Piped
// output and log files get the query as one run of text in the single
// MaprTable.RawQuery color, as before the highlighting, so that a search
// for the query (grep 'from STATS select' out) still finds it.
func rawQueryTokenColors(cfg config.RuntimeConfig, stdoutTerminal bool) bool {
	if !stdoutTerminal {
		return false
	}
	if cfg.Common == nil {
		return true
	}
	switch strings.ToLower(cfg.Common.Logger) {
	case "file":
		return false
	case "fout":
		return cfg.Client == nil || !cfg.Client.LogPayload
	default:
		return true
	}
}

type clientOutputFormatter struct {
	interruptEnabled bool
	interruptStyle   textStyle
	rawQueryEnabled  bool
	rawQueryStyle    textStyle
	// rawQueryTokens paints the raw query word by word with queryTheme
	// instead of in the single rawQueryStyle (rawQueryTokenColors).
	rawQueryTokens bool
	// queryTheme colors the tokens of the raw query (PaintMaprRawQuery).
	queryTheme   config.InteractiveTermColors
	maprRenderer mapr.ResultRenderer
}

// newClientOutputFormatter returns the formatter of the client output for
// the client configuration clientCfg; rawQueryTokens is the decision of
// rawQueryTokenColors.
func newClientOutputFormatter(clientCfg *config.ClientConfig, rawQueryTokens bool) *clientOutputFormatter {
	formatter := &clientOutputFormatter{
		maprRenderer: mapr.PlainResultRenderer(),
	}
	if clientCfg == nil || !clientCfg.TermColorsEnable {
		return formatter
	}

	formatter.interruptEnabled = true
	formatter.rawQueryEnabled = true
	formatter.interruptStyle = textStyle{
		fg:   clientCfg.TermColors.Client.ClientFg,
		bg:   clientCfg.TermColors.Client.ClientBg,
		attr: clientCfg.TermColors.Client.ClientAttr,
	}
	formatter.rawQueryTokens = rawQueryTokens
	formatter.queryTheme = clientCfg.TermColors.Interactive
	formatter.rawQueryStyle = textStyle{
		fg:   clientCfg.TermColors.MaprTable.RawQueryFg,
		bg:   clientCfg.TermColors.MaprTable.RawQueryBg,
		attr: clientCfg.TermColors.MaprTable.RawQueryAttr,
	}
	formatter.maprRenderer = maprTerminalRenderer{
		header: textStyle{
			fg:   clientCfg.TermColors.MaprTable.HeaderFg,
			bg:   clientCfg.TermColors.MaprTable.HeaderBg,
			attr: clientCfg.TermColors.MaprTable.HeaderAttr,
		},
		headerDelimiter: textStyle{
			fg:   clientCfg.TermColors.MaprTable.HeaderDelimiterFg,
			bg:   clientCfg.TermColors.MaprTable.HeaderDelimiterBg,
			attr: clientCfg.TermColors.MaprTable.HeaderDelimiterAttr,
		},
		headerSortAttr:  clientCfg.TermColors.MaprTable.HeaderSortKeyAttr,
		headerGroupAttr: clientCfg.TermColors.MaprTable.HeaderGroupKeyAttr,
		data: textStyle{
			fg:   clientCfg.TermColors.MaprTable.DataFg,
			bg:   clientCfg.TermColors.MaprTable.DataBg,
			attr: clientCfg.TermColors.MaprTable.DataAttr,
		},
		dataDelimiter: textStyle{
			fg:   clientCfg.TermColors.MaprTable.DelimiterFg,
			bg:   clientCfg.TermColors.MaprTable.DelimiterBg,
			attr: clientCfg.TermColors.MaprTable.DelimiterAttr,
		},
	}

	return formatter
}

func (f *clientOutputFormatter) FormatInterruptMessage(index int, message string) string {
	if index > 0 && f.interruptEnabled {
		return color.PaintStrWithAttr(message, f.interruptStyle.fg, f.interruptStyle.bg, f.interruptStyle.attr)
	}
	return " " + message
}

// PaintMaprRawQuery paints the raw query printed above a MapReduce result
// table. On a terminal (rawQueryTokens) it is highlighted by token kind
// with the interactive theme (queryHighlighter), on the MaprTable.RawQuery
// colors, which are the colors of the text between the tokens (and of
// words the parser does not know); otherwise it is one run of text in the
// MaprTable.RawQuery colors. Without colors it is returned unchanged.
func (f *clientOutputFormatter) PaintMaprRawQuery(rawQuery string) string {
	if !f.rawQueryEnabled {
		return rawQuery
	}
	if !f.rawQueryTokens {
		return color.PaintStrWithAttr(rawQuery, f.rawQueryStyle.fg, f.rawQueryStyle.bg, f.rawQueryStyle.attr)
	}
	under := string(f.rawQueryStyle.fg) + string(f.rawQueryStyle.bg)
	return queryHighlighter{theme: f.queryTheme, base: under + string(f.rawQueryStyle.attr), under: under}.plain(rawQuery)
}

func (f *clientOutputFormatter) MaprResultRenderer() mapr.ResultRenderer {
	if f == nil || f.maprRenderer == nil {
		return mapr.PlainResultRenderer()
	}
	return f.maprRenderer
}

type textStyle struct {
	fg   color.FgColor
	bg   color.BgColor
	attr color.Attribute
}

type maprTerminalRenderer struct {
	header          textStyle
	headerDelimiter textStyle
	headerSortAttr  color.Attribute
	headerGroupAttr color.Attribute
	data            textStyle
	dataDelimiter   textStyle
}

func (r maprTerminalRenderer) WriteHeaderEntry(sb *strings.Builder, text string, isSortKey, isGroupKey bool) {
	attrs := []color.Attribute{r.header.attr}
	if isSortKey {
		attrs = append(attrs, r.headerSortAttr)
	}
	if isGroupKey {
		attrs = append(attrs, r.headerGroupAttr)
	}
	color.PaintWithAttrs(sb, text, r.header.fg, r.header.bg, attrs)
}

func (r maprTerminalRenderer) WriteHeaderDelimiter(sb *strings.Builder, text string) {
	color.PaintWithAttr(sb, text, r.headerDelimiter.fg, r.headerDelimiter.bg, r.headerDelimiter.attr)
}

func (r maprTerminalRenderer) WriteDataEntry(sb *strings.Builder, text string) {
	color.PaintWithAttr(sb, text, r.data.fg, r.data.bg, r.data.attr)
}

func (r maprTerminalRenderer) WriteDataDelimiter(sb *strings.Builder, text string) {
	color.PaintWithAttr(sb, text, r.dataDelimiter.fg, r.dataDelimiter.bg, r.dataDelimiter.attr)
}

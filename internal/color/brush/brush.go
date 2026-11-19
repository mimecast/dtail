package brush

import (
	"strings"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/protocol"
)

var defaultBrush = &Brush{theme: config.DefaultTermColors()}

// Brush renders protocol messages with a fixed terminal palette.
type Brush struct {
	theme config.TermColors
}

// New returns a Brush that renders with theme.
func New(theme config.TermColors) *Brush {
	return &Brush{theme: theme}
}

// Default returns the immutable Brush used by compatibility paths.
func Default() *Brush {
	return defaultBrush
}

func (b *Brush) paintSeverity(sb *strings.Builder, text string) bool {
	switch {
	case strings.HasPrefix(text, "WARN"):
		color.PaintWithAttr(sb, text,
			b.theme.Common.SeverityWarnFg,
			b.theme.Common.SeverityWarnBg,
			b.theme.Common.SeverityWarnAttr)

	case strings.HasPrefix(text, "ERROR"):
		color.PaintWithAttr(sb, text,
			b.theme.Common.SeverityErrorFg,
			b.theme.Common.SeverityErrorBg,
			b.theme.Common.SeverityErrorAttr)

	case strings.HasPrefix(text, "FATAL"):
		color.PaintWithAttr(sb, text,
			b.theme.Common.SeverityFatalFg,
			b.theme.Common.SeverityFatalBg,
			b.theme.Common.SeverityFatalAttr)

	default:
		return false
	}
	return true
}

func (b *Brush) paintRemote(sb *strings.Builder, line string) {
	decoded, err := protocol.DecodeLine(line)
	if err != nil {
		// Malformed or short frame (e.g. from an older/buggy server):
		// fall back to the plain-text default branch instead of
		// indexing out of range.
		b.paintDefault(sb, line)
		return
	}

	color.PaintWithAttr(sb, protocol.LineMessageID,
		b.theme.Remote.RemoteFg,
		b.theme.Remote.RemoteBg,
		b.theme.Remote.RemoteAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		b.theme.Remote.DelimiterFg,
		b.theme.Remote.DelimiterBg,
		b.theme.Remote.DelimiterAttr)

	color.PaintWithAttr(sb, decoded.Hostname,
		b.theme.Remote.HostnameFg,
		b.theme.Remote.HostnameBg,
		b.theme.Remote.HostnameAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		b.theme.Remote.DelimiterFg,
		b.theme.Remote.DelimiterBg,
		b.theme.Remote.DelimiterAttr)

	if decoded.TransmittedPercent == "100" {
		color.PaintWithAttr(sb, decoded.TransmittedPercent,
			b.theme.Remote.StatsOkFg,
			b.theme.Remote.StatsOkBg,
			b.theme.Remote.StatsOkAttr)
	} else {
		color.PaintWithAttr(sb, decoded.TransmittedPercent,
			b.theme.Remote.StatsWarnFg,
			b.theme.Remote.StatsWarnBg,
			b.theme.Remote.StatsWarnAttr)
	}

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		b.theme.Remote.DelimiterFg,
		b.theme.Remote.DelimiterBg,
		b.theme.Remote.DelimiterAttr)

	color.PaintWithAttr(sb, decoded.Number,
		b.theme.Remote.CountFg,
		b.theme.Remote.CountBg,
		b.theme.Remote.CountAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		b.theme.Remote.DelimiterFg,
		b.theme.Remote.DelimiterBg,
		b.theme.Remote.DelimiterAttr)

	color.PaintWithAttr(sb, decoded.SourceID,
		b.theme.Remote.IDFg,
		b.theme.Remote.IDBg,
		b.theme.Remote.IDAttr)
	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		b.theme.Remote.DelimiterFg,
		b.theme.Remote.DelimiterBg,
		b.theme.Remote.DelimiterAttr)

	if b.paintSeverity(sb, decoded.Content) {
		return
	}
	color.PaintWithAttr(sb, decoded.Content,
		b.theme.Remote.TextFg,
		b.theme.Remote.TextBg,
		b.theme.Remote.TextAttr)
}

func (b *Brush) paintClient(sb *strings.Builder, line string) {
	splitted := strings.SplitN(line, protocol.FieldDelimiter, 3)
	if len(splitted) < 3 {
		b.paintDefault(sb, line)
		return
	}

	color.PaintWithAttr(sb, splitted[0],
		b.theme.Client.ClientFg,
		b.theme.Client.ClientBg,
		b.theme.Client.ClientAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		b.theme.Client.DelimiterFg,
		b.theme.Client.DelimiterBg,
		b.theme.Client.DelimiterAttr)

	color.PaintWithAttr(sb, splitted[1],
		b.theme.Client.HostnameFg,
		b.theme.Client.HostnameBg,
		b.theme.Client.HostnameAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		b.theme.Client.DelimiterFg,
		b.theme.Client.DelimiterBg,
		b.theme.Client.DelimiterAttr)

	if b.paintSeverity(sb, splitted[2]) {
		return
	}

	color.PaintWithAttr(sb, splitted[2],
		b.theme.Client.TextFg,
		b.theme.Client.TextBg,
		b.theme.Client.TextAttr)
}

func (b *Brush) paintServer(sb *strings.Builder, line string) {
	decoded, err := protocol.DecodeMessage(line)
	if err != nil || decoded.Kind != protocol.MessageServer {
		b.paintDefault(sb, line)
		return
	}

	color.PaintWithAttr(sb, protocol.ServerMessageID,
		b.theme.Server.ServerFg,
		b.theme.Server.ServerBg,
		b.theme.Server.ServerAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		b.theme.Server.DelimiterFg,
		b.theme.Server.DelimiterBg,
		b.theme.Server.DelimiterAttr)

	color.PaintWithAttr(sb, decoded.Hostname,
		b.theme.Server.HostnameFg,
		b.theme.Server.HostnameBg,
		b.theme.Server.HostnameAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		b.theme.Server.DelimiterFg,
		b.theme.Server.DelimiterBg,
		b.theme.Server.DelimiterAttr)

	if b.paintSeverity(sb, decoded.Content) {
		return
	}

	color.PaintWithAttr(sb, decoded.Content,
		b.theme.Server.TextFg,
		b.theme.Server.TextBg,
		b.theme.Server.TextAttr)
}

// Colorfy renders a line based on its protocol fields and content.
func (b *Brush) Colorfy(line string) string {
	if b == nil {
		b = defaultBrush
	}
	sb := pool.BuilderBuffer.Get().(*strings.Builder)
	defer pool.RecycleBuilderBuffer(sb)

	switch {
	case strings.HasPrefix(line, protocol.LineMessageID):
		b.paintRemote(sb, line)

	case strings.HasPrefix(line, "CLIENT"):
		b.paintClient(sb, line)

	case strings.HasPrefix(line, protocol.ServerMessageID):
		b.paintServer(sb, line)

	default:
		b.paintDefault(sb, line)
	}
	return sb.String()
}

// Colorfy renders a line with the standard palette.
//
// Deprecated: construct a Brush with New and inject it into the caller.
func Colorfy(line string) string {
	return Default().Colorfy(line)
}

// paintDefault writes the line using the default (uncoloured) attributes.
// It is the fallback used both by Colorfy's default branch and by the
// paint* functions when the protocol frame is too short to decode safely.
func (*Brush) paintDefault(sb *strings.Builder, line string) {
	color.PaintWithAttr(sb, line,
		color.FgDefault,
		color.BgDefault,
		color.AttrNone)
}

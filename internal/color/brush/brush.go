package brush

import (
	"strings"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/protocol"
)

func paintSeverity(sb *strings.Builder, text string) bool {
	switch {
	case strings.HasPrefix(text, "WARN"):
		color.PaintWithAttr(sb, text,
			config.Client.TermColors.Common.SeverityWarnFg,
			config.Client.TermColors.Common.SeverityWarnBg,
			config.Client.TermColors.Common.SeverityWarnAttr)

	case strings.HasPrefix(text, "ERROR"):
		color.PaintWithAttr(sb, text,
			config.Client.TermColors.Common.SeverityErrorFg,
			config.Client.TermColors.Common.SeverityErrorBg,
			config.Client.TermColors.Common.SeverityErrorAttr)

	case strings.HasPrefix(text, "FATAL"):
		color.PaintWithAttr(sb, text,
			config.Client.TermColors.Common.SeverityFatalFg,
			config.Client.TermColors.Common.SeverityFatalBg,
			config.Client.TermColors.Common.SeverityFatalAttr)

	default:
		return false
	}

	return true
}

func paintRemote(sb *strings.Builder, line string) {
	splitted := strings.SplitN(line, protocol.FieldDelimiter, 6)

	color.PaintWithAttr(sb, splitted[0],
		config.Client.TermColors.Remote.RemoteFg,
		config.Client.TermColors.Remote.RemoteBg,
		config.Client.TermColors.Remote.RemoteAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		config.Client.TermColors.Remote.DelimiterFg,
		config.Client.TermColors.Remote.DelimiterBg,
		config.Client.TermColors.Remote.DelimiterAttr)

	color.PaintWithAttr(sb, splitted[1],
		config.Client.TermColors.Remote.HostnameFg,
		config.Client.TermColors.Remote.HostnameBg,
		config.Client.TermColors.Remote.HostnameAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		config.Client.TermColors.Remote.DelimiterFg,
		config.Client.TermColors.Remote.DelimiterBg,
		config.Client.TermColors.Remote.DelimiterAttr)

	if splitted[2] == "100" {
		color.PaintWithAttr(sb, splitted[2],
			config.Client.TermColors.Remote.StatsOkFg,
			config.Client.TermColors.Remote.StatsOkBg,
			config.Client.TermColors.Remote.StatsOkAttr)
	} else {
		color.PaintWithAttr(sb, splitted[2],
			config.Client.TermColors.Remote.StatsWarnFg,
			config.Client.TermColors.Remote.StatsWarnBg,
			config.Client.TermColors.Remote.StatsWarnAttr)
	}

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		config.Client.TermColors.Remote.DelimiterFg,
		config.Client.TermColors.Remote.DelimiterBg,
		config.Client.TermColors.Remote.DelimiterAttr)

	color.PaintWithAttr(sb, splitted[3],
		config.Client.TermColors.Remote.CountFg,
		config.Client.TermColors.Remote.CountBg,
		config.Client.TermColors.Remote.CountAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		config.Client.TermColors.Remote.DelimiterFg,
		config.Client.TermColors.Remote.DelimiterBg,
		config.Client.TermColors.Remote.DelimiterAttr)

	color.PaintWithAttr(sb, splitted[4],
		config.Client.TermColors.Remote.IdFg,
		config.Client.TermColors.Remote.IdBg,
		config.Client.TermColors.Remote.IdAttr)
	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		config.Client.TermColors.Remote.DelimiterFg,
		config.Client.TermColors.Remote.DelimiterBg,
		config.Client.TermColors.Remote.DelimiterAttr)

	if paintSeverity(sb, splitted[5]) {
		return
	}
	color.PaintWithAttr(sb, splitted[5],
		config.Client.TermColors.Remote.TextFg,
		config.Client.TermColors.Remote.TextBg,
		config.Client.TermColors.Remote.TextAttr)
}

func paintClient(sb *strings.Builder, line string) {
	splitted := strings.SplitN(line, protocol.FieldDelimiter, 3)

	color.PaintWithAttr(sb, splitted[0],
		config.Client.TermColors.Client.ClientFg,
		config.Client.TermColors.Client.ClientBg,
		config.Client.TermColors.Client.ClientAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		config.Client.TermColors.Client.DelimiterFg,
		config.Client.TermColors.Client.DelimiterBg,
		config.Client.TermColors.Client.DelimiterAttr)

	color.PaintWithAttr(sb, splitted[1],
		config.Client.TermColors.Client.HostnameFg,
		config.Client.TermColors.Client.HostnameBg,
		config.Client.TermColors.Client.HostnameAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		config.Client.TermColors.Client.DelimiterFg,
		config.Client.TermColors.Client.DelimiterBg,
		config.Client.TermColors.Client.DelimiterAttr)

	if paintSeverity(sb, splitted[2]) {
		return
	}

	color.PaintWithAttr(sb, splitted[2],
		config.Client.TermColors.Client.TextFg,
		config.Client.TermColors.Client.TextBg,
		config.Client.TermColors.Client.TextAttr)
}

func paintServer(sb *strings.Builder, line string) {
	splitted := strings.SplitN(line, protocol.FieldDelimiter, 3)

	color.PaintWithAttr(sb, splitted[0],
		config.Client.TermColors.Server.ServerFg,
		config.Client.TermColors.Server.ServerBg,
		config.Client.TermColors.Server.ServerAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		config.Client.TermColors.Server.DelimiterFg,
		config.Client.TermColors.Server.DelimiterBg,
		config.Client.TermColors.Server.DelimiterAttr)

	color.PaintWithAttr(sb, splitted[1],
		config.Client.TermColors.Server.HostnameFg,
		config.Client.TermColors.Server.HostnameBg,
		config.Client.TermColors.Server.HostnameAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		config.Client.TermColors.Server.DelimiterFg,
		config.Client.TermColors.Server.DelimiterBg,
		config.Client.TermColors.Server.DelimiterAttr)

	if paintSeverity(sb, splitted[2]) {
		return
	}

	color.PaintWithAttr(sb, splitted[2],
		config.Client.TermColors.Server.TextFg,
		config.Client.TermColors.Server.TextBg,
		config.Client.TermColors.Server.TextAttr)
}

// Colorfy a given line based on the line's content.
func Colorfy(line string) string {
	sb := strings.Builder{}

	switch {
	case strings.HasPrefix(line, "REMOTE"):
		paintRemote(&sb, line)

	case strings.HasPrefix(line, "CLIENT"):
		paintClient(&sb, line)

	case strings.HasPrefix(line, "SERVER"):
		paintServer(&sb, line)

	default:
		color.PaintWithAttr(&sb, line,
			color.FgDefault,
			color.BgDefault,
			color.AttrNone)
	}

	return sb.String()
}

package brush

import (
	"strings"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/protocol"
)

// Add some color to log lines received from remote servers.
func paintRemote(sb *strings.Builder, line string) {
	splitted := strings.SplitN(line, protocol.FieldDelimiter, 6)

	color.PaintWithAttr(sb, splitted[0],
		config.Client.TermColors.RemoteStrFg,
		config.Client.TermColors.RemoteStrBg,
		config.Client.TermColors.RemoteStrAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		config.Client.TermColors.DelimiterFg,
		config.Client.TermColors.DelimiterBg,
		config.Client.TermColors.DelimiterAttr)

	color.PaintWithAttr(sb, splitted[1],
		config.Client.TermColors.RemoteServerFg,
		config.Client.TermColors.RemoteServerBg,
		config.Client.TermColors.RemoteServerAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		config.Client.TermColors.DelimiterFg,
		config.Client.TermColors.DelimiterBg,
		config.Client.TermColors.DelimiterAttr)

	if splitted[2] == "100" {
		color.PaintWithAttr(sb, splitted[2],
			config.Client.TermColors.RemoteStatsOkFg,
			config.Client.TermColors.RemoteStatsOkBg,
			config.Client.TermColors.RemoteStatsOkAttr)
	} else {
		color.PaintWithAttr(sb, splitted[2],
			config.Client.TermColors.RemoteStatsWarnFg,
			config.Client.TermColors.RemoteStatsWarnBg,
			config.Client.TermColors.RemoteStatsWarnAttr)
	}

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		config.Client.TermColors.DelimiterFg,
		config.Client.TermColors.DelimiterBg,
		config.Client.TermColors.DelimiterAttr)

	color.PaintWithAttr(sb, splitted[3],
		config.Client.TermColors.RemoteCountFg,
		config.Client.TermColors.RemoteCountBg,
		config.Client.TermColors.RemoteCountAttr)

	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		config.Client.TermColors.DelimiterFg,
		config.Client.TermColors.DelimiterBg,
		config.Client.TermColors.DelimiterAttr)

	color.PaintWithAttr(sb, splitted[4],
		config.Client.TermColors.RemoteIdFg,
		config.Client.TermColors.RemoteIdBg,
		config.Client.TermColors.RemoteIdAttr)
	color.PaintWithAttr(sb, protocol.FieldDelimiter,
		config.Client.TermColors.DelimiterFg,
		config.Client.TermColors.DelimiterBg,
		config.Client.TermColors.DelimiterAttr)

	log := splitted[5]

	switch {
	case strings.HasPrefix(log, "WARN"):
		color.PaintWithAttr(sb, log,
			config.Client.TermColors.RemoteWarnFg,
			config.Client.TermColors.RemoteWarnBg,
			config.Client.TermColors.RemoteWarnAttr)

	case strings.HasPrefix(log, "ERROR"):
		color.PaintWithAttr(sb, log,
			config.Client.TermColors.RemoteErrorFg,
			config.Client.TermColors.RemoteErrorBg,
			config.Client.TermColors.RemoteErrorAttr)

	case strings.HasPrefix(log, "FATAL"):
		color.PaintWithAttr(sb, log,
			config.Client.TermColors.RemoteFatalFg,
			config.Client.TermColors.RemoteFatalBg,
			config.Client.TermColors.RemoteFatalAttr)

	case strings.HasPrefix(log, "DEBUG"):
		color.PaintWithAttr(sb, log,
			config.Client.TermColors.RemoteDebugFg,
			config.Client.TermColors.RemoteDebugBg,
			config.Client.TermColors.RemoteDebugAttr)

	case strings.HasPrefix(log, "TRACE"):
		color.PaintWithAttr(sb, log,
			config.Client.TermColors.RemoteTraceFg,
			config.Client.TermColors.RemoteTraceBg,
			config.Client.TermColors.RemoteTraceAttr)

	default:
		color.PaintWithAttr(sb, log,
			config.Client.TermColors.RemoteTextFg,
			config.Client.TermColors.RemoteTextBg,
			config.Client.TermColors.RemoteTextAttr)
	}
}

// Colorfy a given line based on the line's content.
func Colorfy(line string) string {
	sb := strings.Builder{}

	switch {
	case strings.HasPrefix(line, "REMOTE"):
		paintRemote(&sb, line)

	case strings.Contains(line, "ERROR"):
		color.PaintWithAttr(&sb, line,
			config.Client.TermColors.ClientErrorFg,
			config.Client.TermColors.ClientErrorBg,
			config.Client.TermColors.ClientErrorAttr)

	case strings.Contains(line, "WARN"):
		color.PaintWithAttr(&sb, line,
			config.Client.TermColors.ClientWarnFg,
			config.Client.TermColors.ClientWarnBg,
			config.Client.TermColors.ClientWarnAttr)

	default:
		color.PaintWithAttr(&sb, line,
			color.FgDefault,
			color.BgDefault,
			color.AttrNone)
	}

	color.ResetWithAttr(&sb)
	return sb.String()
}

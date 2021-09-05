package config

import "github.com/mimecast/dtail/internal/color"

// ClientColorConfig allows to override the default terminal color color.
type termColors struct {
	ClientErrorAttr     color.Attribute
	ClientErrorBg       color.BgColor
	ClientErrorFg       color.FgColor
	ClientWarnAttr      color.Attribute
	ClientWarnBg        color.BgColor
	ClientWarnFg        color.FgColor
	DelimiterAttr       color.Attribute
	DelimiterBg         color.BgColor
	DelimiterFg         color.FgColor
	RemoteCountAttr     color.Attribute
	RemoteCountBg       color.BgColor
	RemoteCountFg       color.FgColor
	RemoteDebugAttr     color.Attribute
	RemoteDebugBg       color.BgColor
	RemoteDebugFg       color.FgColor
	RemoteErrorAttr     color.Attribute
	RemoteErrorBg       color.BgColor
	RemoteErrorFg       color.FgColor
	RemoteFatalAttr     color.Attribute
	RemoteFatalBg       color.BgColor
	RemoteFatalFg       color.FgColor
	RemoteIdAttr        color.Attribute
	RemoteIdBg          color.BgColor
	RemoteIdFg          color.FgColor
	RemoteServerAttr    color.Attribute
	RemoteServerBg      color.BgColor
	RemoteServerFg      color.FgColor
	RemoteStatsOkAttr   color.Attribute
	RemoteStatsOkBg     color.BgColor
	RemoteStatsOkFg     color.FgColor
	RemoteStatsWarnAttr color.Attribute
	RemoteStatsWarnBg   color.BgColor
	RemoteStatsWarnFg   color.FgColor
	RemoteStrAttr       color.Attribute
	RemoteStrBg         color.BgColor
	RemoteStrFg         color.FgColor
	RemoteTextAttr      color.Attribute
	RemoteTextBg        color.BgColor
	RemoteTextFg        color.FgColor
	RemoteTraceAttr     color.Attribute
	RemoteTraceBg       color.BgColor
	RemoteTraceFg       color.FgColor
	RemoteWarnAttr      color.Attribute
	RemoteWarnBg        color.BgColor
	RemoteWarnFg        color.FgColor
}

// ClientConfig represents a DTail client configuration (empty as of now as there
// are no available config options yet, but that may changes in the future).
type ClientConfig struct {
	TermColorsEnable bool
	TermColors       termColors `json:",omitempty"`
}

// Create a new default client configuration.
func newDefaultClientConfig() *ClientConfig {
	return &ClientConfig{
		TermColorsEnable: true,
		TermColors: termColors{
			ClientErrorAttr:     color.AttrBold,
			ClientErrorBg:       color.BgBlack,
			ClientErrorFg:       color.FgRed,
			ClientWarnAttr:      color.AttrNone,
			ClientWarnBg:        color.BgBlack,
			ClientWarnFg:        color.FgMagenta,
			DelimiterAttr:       color.AttrDim,
			DelimiterBg:         color.BgBlue,
			DelimiterFg:         color.FgCyan,
			RemoteCountAttr:     color.AttrDim,
			RemoteCountBg:       color.BgBlue,
			RemoteCountFg:       color.FgGreen,
			RemoteDebugAttr:     color.AttrBold,
			RemoteDebugBg:       color.BgBlack,
			RemoteDebugFg:       color.FgGreen,
			RemoteErrorAttr:     color.AttrBold,
			RemoteErrorBg:       color.BgRed,
			RemoteErrorFg:       color.FgWhite,
			RemoteFatalAttr:     color.AttrBlink,
			RemoteFatalBg:       color.BgRed,
			RemoteFatalFg:       color.FgWhite,
			RemoteIdAttr:        color.AttrDim,
			RemoteIdBg:          color.BgBlue,
			RemoteIdFg:          color.FgWhite,
			RemoteServerAttr:    color.AttrBold,
			RemoteServerBg:      color.BgBlue,
			RemoteServerFg:      color.FgWhite,
			RemoteStatsOkAttr:   color.AttrNone,
			RemoteStatsOkBg:     color.BgGreen,
			RemoteStatsOkFg:     color.FgBlue,
			RemoteStatsWarnAttr: color.AttrNone,
			RemoteStatsWarnBg:   color.BgRed,
			RemoteStatsWarnFg:   color.FgWhite,
			RemoteStrAttr:       color.AttrDim,
			RemoteStrBg:         color.BgBlue,
			RemoteStrFg:         color.FgWhite,
			RemoteTextAttr:      color.AttrNone,
			RemoteTextBg:        color.BgBlack,
			RemoteTextFg:        color.FgWhite,
			RemoteTraceAttr:     color.AttrBold,
			RemoteTraceBg:       color.BgGreen,
			RemoteTraceFg:       color.FgWhite,
			RemoteWarnAttr:      color.AttrBold,
			RemoteWarnBg:        color.BgYellow,
			RemoteWarnFg:        color.FgWhite,
		},
	}
}

package config

import "github.com/mimecast/dtail/internal/color"

// ClientColorConfig allows to override the default terminal color color.
type termColors struct {
	ClientErrorAttr color.Attribute
	ClientErrorBg   color.BgColor
	ClientErrorFg   color.FgColor

	ClientStatsAttr color.Attribute
	ClientStatsBg   color.BgColor
	ClientStatsFg   color.FgColor

	ClientWarnAttr color.Attribute
	ClientWarnBg   color.BgColor
	ClientWarnFg   color.FgColor

	RemoteDebugAttr color.Attribute
	RemoteDebugBg   color.BgColor
	RemoteDebugFg   color.FgColor

	RemoteErrorAttr color.Attribute
	RemoteErrorBg   color.BgColor
	RemoteErrorFg   color.FgColor

	RemoteFatalAttr color.Attribute
	RemoteFatalBg   color.BgColor
	RemoteFatalFg   color.FgColor

	RemoteStatsOkAttr color.Attribute
	RemoteStatsOkBg   color.BgColor
	RemoteStatsOkFg   color.FgColor

	RemoteStatsWarnAttr color.Attribute
	RemoteStatsWarnBg   color.BgColor
	RemoteStatsWarnFg   color.FgColor

	RemoteTextAttr color.Attribute
	RemoteTextBg   color.BgColor
	RemoteTextFg   color.FgColor

	RemoteTraceAttr color.Attribute
	RemoteTraceBg   color.BgColor
	RemoteTraceFg   color.FgColor

	RemoteWarnAttr color.Attribute
	RemoteWarnBg   color.BgColor
	RemoteWarnFg   color.FgColor
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
			ClientErrorAttr: color.AttrBold,
			ClientErrorBg:   color.BgBlack,
			ClientErrorFg:   color.FgRed,

			ClientStatsAttr: color.AttrDim,
			ClientStatsBg:   color.BgBlue,
			ClientStatsFg:   color.FgWhite,

			ClientWarnAttr: color.AttrNone,
			ClientWarnBg:   color.BgBlack,
			ClientWarnFg:   color.FgMagenta,

			RemoteDebugAttr: color.AttrNone,
			RemoteDebugBg:   color.BgGreen,
			RemoteDebugFg:   color.FgBlack,

			RemoteErrorAttr: color.AttrBold,
			RemoteErrorBg:   color.BgRed,
			RemoteErrorFg:   color.FgWhite,

			RemoteFatalAttr: color.AttrBlink,
			RemoteFatalBg:   color.BgRed,
			RemoteFatalFg:   color.FgWhite,

			RemoteStatsOkAttr: color.AttrNone,
			RemoteStatsOkBg:   color.BgGreen,
			RemoteStatsOkFg:   color.FgBlack,

			RemoteStatsWarnAttr: color.AttrNone,
			RemoteStatsWarnBg:   color.BgRed,
			RemoteStatsWarnFg:   color.FgWhite,

			RemoteTextAttr: color.AttrNone,
			RemoteTextBg:   color.BgBlack,
			RemoteTextFg:   color.FgWhite,

			RemoteTraceAttr: color.AttrBold,
			RemoteTraceBg:   color.BgGreen,
			RemoteTraceFg:   color.FgWhite,

			RemoteWarnAttr: color.AttrBold,
			RemoteWarnBg:   color.BgYellow,
			RemoteWarnFg:   color.FgWhite,
		},
	}
}

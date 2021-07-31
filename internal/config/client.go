package config

import "github.com/mimecast/dtail/internal/color"

// ClientColorConfig allows to override the default terminal color color.
type termColors struct {
	RemoteTextFg      color.Color
	RemoteStatsOkBg   color.Color
	RemoteStatsWarnBg color.Color
	RemoteTraceBg     color.Color
	RemoteDebugBg     color.Color
	RemoteWarnBg      color.Color
	RemoteErrorBg     color.Color
	RemoteFatalBg     color.Color
	RemoteFatalAttr   color.Attribute
	ClientStatsBg     color.Color
	ClientWarnFg      color.Color
	ClientErrorFg     color.Color
}

// ClientConfig represents a DTail client configuration (empty as of now as there
// are no available config options yet, but that may changes in the future).
type ClientConfig struct {
	TermColorsEnabled bool
	TermColors        termColors `json:",omitempty"`
}

// Create a new default client configuration.
func newDefaultClientConfig() *ClientConfig {
	return &ClientConfig{
		TermColorsEnabled: true,
		TermColors: termColors{
			RemoteTextFg:      color.LightGray,
			RemoteStatsOkBg:   color.BgGreen,
			RemoteStatsWarnBg: color.BgRed,
			RemoteTraceBg:     color.BgYellow,
			RemoteDebugBg:     color.BgYellow,
			RemoteWarnBg:      color.BgYellow,
			RemoteErrorBg:     color.BgRed,
			RemoteFatalBg:     color.BgRed,
			RemoteFatalAttr:   color.Bold,
			ClientStatsBg:     color.BgBlue,
			ClientWarnFg:      color.Purple,
			ClientErrorFg:     color.BgRed,
		},
	}
}

package config

import (
	"os"
	"path/filepath"

	"github.com/mimecast/dtail/internal/color"
)

type remoteTermColors struct {
	DelimiterAttr color.Attribute
	DelimiterBg   color.BgColor
	DelimiterFg   color.FgColor
	RemoteAttr    color.Attribute
	RemoteBg      color.BgColor
	RemoteFg      color.FgColor
	CountAttr     color.Attribute
	CountBg       color.BgColor
	CountFg       color.FgColor
	HostnameAttr  color.Attribute
	HostnameBg    color.BgColor
	HostnameFg    color.FgColor
	IDAttr        color.Attribute
	IDBg          color.BgColor
	IDFg          color.FgColor
	StatsOkAttr   color.Attribute
	StatsOkBg     color.BgColor
	StatsOkFg     color.FgColor
	StatsWarnAttr color.Attribute
	StatsWarnBg   color.BgColor
	StatsWarnFg   color.FgColor
	TextAttr      color.Attribute
	TextBg        color.BgColor
	TextFg        color.FgColor
}

type clientTermColors struct {
	DelimiterAttr color.Attribute
	DelimiterBg   color.BgColor
	DelimiterFg   color.FgColor
	ClientAttr    color.Attribute
	ClientBg      color.BgColor
	ClientFg      color.FgColor
	HostnameAttr  color.Attribute
	HostnameBg    color.BgColor
	HostnameFg    color.FgColor
	TextAttr      color.Attribute
	TextBg        color.BgColor
	TextFg        color.FgColor
}

type serverTermColors struct {
	DelimiterAttr color.Attribute
	DelimiterBg   color.BgColor
	DelimiterFg   color.FgColor
	ServerAttr    color.Attribute
	ServerBg      color.BgColor
	ServerFg      color.FgColor
	HostnameAttr  color.Attribute
	HostnameBg    color.BgColor
	HostnameFg    color.FgColor
	TextAttr      color.Attribute
	TextBg        color.BgColor
	TextFg        color.FgColor
}

type commonTermColors struct {
	SeverityErrorAttr color.Attribute
	SeverityErrorBg   color.BgColor
	SeverityErrorFg   color.FgColor
	SeverityFatalAttr color.Attribute
	SeverityFatalBg   color.BgColor
	SeverityFatalFg   color.FgColor
	SeverityWarnAttr  color.Attribute
	SeverityWarnBg    color.BgColor
	SeverityWarnFg    color.FgColor
}

type maprTableTermColors struct {
	DataAttr            color.Attribute
	DataBg              color.BgColor
	DataFg              color.FgColor
	DelimiterAttr       color.Attribute
	DelimiterBg         color.BgColor
	DelimiterFg         color.FgColor
	HeaderAttr          color.Attribute
	HeaderBg            color.BgColor
	HeaderDelimiterAttr color.Attribute
	HeaderDelimiterBg   color.BgColor
	HeaderDelimiterFg   color.FgColor
	HeaderFg            color.FgColor
	HeaderGroupKeyAttr  color.Attribute
	HeaderSortKeyAttr   color.Attribute
	RawQueryAttr        color.Attribute
	RawQueryBg          color.BgColor
	RawQueryFg          color.FgColor
}

// TermColors contains the terminal palette used to render protocol messages.
type TermColors struct {
	Remote    remoteTermColors
	Client    clientTermColors
	Server    serverTermColors
	Common    commonTermColors
	MaprTable maprTableTermColors
}

// ClientConfig represents a DTail client configuration (empty as of now as there
// are no available config options yet, but that may changes in the future).
type ClientConfig struct {
	TermColorsEnable bool       `json:",omitempty"`
	TermColors       TermColors `json:",omitempty"`
	AuthKeyPath      string     `json:",omitempty"`
	AuthKeyDisable   bool       `json:",omitempty"`
	// KnownHostsPath selects the OpenSSH known_hosts file. An empty value uses
	// the current user's ~/.ssh/known_hosts file.
	KnownHostsPath string `json:",omitempty"`
	// LogPayload opts in to teeing the full retrieved payload (the bulk
	// dcat/dgrep/dtail output) into the daily client log file. Default false:
	// the file keeps diagnostics/audit lines only, so a large read no longer
	// grows the daily log by the full payload size. Payload always still goes
	// to stdout/terminal regardless of this setting. Only affects the default
	// "fout" logger (stdout+file); see docs for other loggers.
	LogPayload bool `json:",omitempty"`
}

// DefaultTermColors returns the standard terminal palette.
func DefaultTermColors() TermColors {
	return defaultTermColors()
}

// Create a new default client configuration.
func newDefaultClientConfig() *ClientConfig {
	return &ClientConfig{
		TermColorsEnable: true,
		AuthKeyPath:      defaultAuthKeyPath(),
		AuthKeyDisable:   false,
		// Default: diagnostics-only client log file. Opt in with --log-payload
		// / Client.LogPayload to restore the legacy full-payload tee.
		LogPayload: false,
		TermColors: DefaultTermColors(),
	}
}

func defaultTermColors() TermColors {
	return TermColors{
		Remote: remoteTermColors{
			DelimiterAttr: color.AttrDim,
			DelimiterBg:   color.BgBlue,
			DelimiterFg:   color.FgCyan,
			RemoteAttr:    color.AttrDim,
			RemoteBg:      color.BgBlue,
			RemoteFg:      color.FgWhite,
			CountAttr:     color.AttrDim,
			CountBg:       color.BgBlue,
			CountFg:       color.FgWhite,
			HostnameAttr:  color.AttrBold,
			HostnameBg:    color.BgBlue,
			HostnameFg:    color.FgWhite,
			IDAttr:        color.AttrDim,
			IDBg:          color.BgBlue,
			IDFg:          color.FgWhite,
			StatsOkAttr:   color.AttrNone,
			StatsOkBg:     color.BgGreen,
			StatsOkFg:     color.FgBlack,
			StatsWarnAttr: color.AttrNone,
			StatsWarnBg:   color.BgRed,
			StatsWarnFg:   color.FgWhite,
			TextAttr:      color.AttrNone,
			TextBg:        color.BgBlack,
			TextFg:        color.FgWhite,
		},
		Client: clientTermColors{
			DelimiterAttr: color.AttrDim,
			DelimiterBg:   color.BgYellow,
			DelimiterFg:   color.FgBlack,
			ClientAttr:    color.AttrDim,
			ClientBg:      color.BgYellow,
			ClientFg:      color.FgBlack,
			HostnameAttr:  color.AttrDim,
			HostnameBg:    color.BgYellow,
			HostnameFg:    color.FgBlack,
			TextAttr:      color.AttrNone,
			TextBg:        color.BgBlack,
			TextFg:        color.FgWhite,
		},
		Server: serverTermColors{
			DelimiterAttr: color.AttrDim,
			DelimiterBg:   color.BgCyan,
			DelimiterFg:   color.FgBlack,
			ServerAttr:    color.AttrDim,
			ServerBg:      color.BgCyan,
			ServerFg:      color.FgBlack,
			HostnameAttr:  color.AttrBold,
			HostnameBg:    color.BgCyan,
			HostnameFg:    color.FgBlack,
			TextAttr:      color.AttrNone,
			TextBg:        color.BgBlack,
			TextFg:        color.FgWhite,
		},
		Common: commonTermColors{
			SeverityErrorAttr: color.AttrBold,
			SeverityErrorBg:   color.BgRed,
			SeverityErrorFg:   color.FgWhite,
			SeverityFatalAttr: color.AttrBold,
			SeverityFatalBg:   color.BgMagenta,
			SeverityFatalFg:   color.FgWhite,
			SeverityWarnAttr:  color.AttrBold,
			SeverityWarnBg:    color.BgBlack,
			SeverityWarnFg:    color.FgWhite,
		},
		MaprTable: maprTableTermColors{
			DataAttr:            color.AttrNone,
			DataBg:              color.BgBlue,
			DataFg:              color.FgWhite,
			DelimiterAttr:       color.AttrDim,
			DelimiterBg:         color.BgBlue,
			DelimiterFg:         color.FgWhite,
			HeaderAttr:          color.AttrBold,
			HeaderBg:            color.BgBlue,
			HeaderFg:            color.FgWhite,
			HeaderDelimiterAttr: color.AttrDim,
			HeaderDelimiterBg:   color.BgBlue,
			HeaderDelimiterFg:   color.FgWhite,
			HeaderSortKeyAttr:   color.AttrUnderline,
			HeaderGroupKeyAttr:  color.AttrReverse,
			RawQueryAttr:        color.AttrDim,
			RawQueryBg:          color.BgBlack,
			RawQueryFg:          color.FgCyan,
		},
	}
}

// defaultAuthKeyPath returns the default path for the SSH auth key based on
// the user's home directory. It tries os.UserHomeDir() first, then falls back
// to the HOME environment variable. If neither resolves to a non-empty path,
// it returns "" so that callers can surface a clear error rather than silently
// using a literal "~/.ssh/id_rsa" that the SSH stack cannot expand.
func defaultAuthKeyPath() string {
	homeDir, err := os.UserHomeDir()
	if err != nil || homeDir == "" {
		homeDir = os.Getenv("HOME")
	}
	if homeDir == "" {
		// Return empty string; callers must check and emit a diagnostic
		// (e.g. "set DTAIL_AUTH_KEY_PATH explicitly") rather than using a
		// path that the SSH library will never find.
		return ""
	}

	return filepath.Join(homeDir, ".ssh", "id_rsa")
}

package config

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/source"
)

func parseTestConfig(t *testing.T, body string) (initializer, error) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "dtail.json")
	writeTestConfig(t, configPath, body)
	in := initializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}
	return in, in.parseConfig(&Args{ConfigFile: configPath})
}

// TestParseConfigConvertsColorNames checks that configured colour names become
// terminal escape codes and that keys the file omits keep their defaults.
func TestParseConfigConvertsColorNames(t *testing.T) {
	in, err := parseTestConfig(t, `{"Client": {"TermColors": {
  "Remote": {"TextFg": "Red", "TextBg": "yellow", "TextAttr": "BOLD"},
  "Common": {"SeverityWarnFg": "Green", "SeverityWarnAttr": ""},
  "MaprTable": {"HeaderSortKeyAttr": "Italic", "RawQueryBg": "Default"}
}}}`)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	want := DefaultTermColors()
	want.Remote.TextFg = color.FgRed
	want.Remote.TextBg = color.BgYellow
	want.Remote.TextAttr = color.AttrBold
	want.Common.SeverityWarnFg = color.FgGreen
	want.Common.SeverityWarnAttr = color.AttrNone
	want.MaprTable.HeaderSortKeyAttr = color.AttrItalic
	want.MaprTable.RawQueryBg = color.BgDefault
	if got := in.Client.TermColors; got != want {
		t.Fatalf("TermColors = %+v, want %+v", got, want)
	}
}

// TestParseConfigKeepsLegacyEscapeColors checks that raw escape sequences from
// older config files are still used unchanged.
func TestParseConfigKeepsLegacyEscapeColors(t *testing.T) {
	in, err := parseTestConfig(t, `{"Client": {"TermColors": {
  "Server": {"TextFg": "\u001b[31m", "TextBg": "\u001b[48;5;17m", "TextAttr": "\u001b[4m"}
}}}`)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	got := in.Client.TermColors.Server
	if got.TextFg != color.FgRed || got.TextBg != color.BgColor("\x1b[48;5;17m") ||
		got.TextAttr != color.AttrUnderline {
		t.Fatalf("legacy escape colours = %q %q %q", got.TextFg, got.TextBg, got.TextAttr)
	}
}

// captureConfigWarnings redirects config warnings into a buffer for the
// duration of the test.
func captureConfigWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := configWarningOutput
	configWarningOutput = &buf
	t.Cleanup(func() { configWarningOutput = previous })
	return &buf
}

// TestSetupFallsBackOnInvalidColorValue checks that an unrecognised colour
// value never fails config loading: the field keeps its default and one
// warning naming the file and the value goes to the warning output (stderr).
func TestSetupFallsBackOnInvalidColorValue(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		get      func(TermColors) string
		want     string
		wantWarn string
	}{
		{name: "foreground", body: `{"Client": {"TermColors": {"Client": {"TextFg": "Purple"}}}}`,
			get: func(c TermColors) string { return string(c.Client.TextFg) }, want: string(DefaultTermColors().Client.TextFg),
			wantWarn: `invalid foreground color "Purple", using the default instead`},
		{name: "background", body: `{"Client": {"TermColors": {"Remote": {"IDBg": "Bleu"}}}}`,
			get: func(c TermColors) string { return string(c.Remote.IDBg) }, want: string(DefaultTermColors().Remote.IDBg),
			wantWarn: `invalid background color "Bleu", using the default instead`},
		{name: "attribute", body: `{"Client": {"TermColors": {"MaprTable": {"HeaderAttr": "Strike"}}}}`,
			get: func(c TermColors) string { return string(c.MaprTable.HeaderAttr) }, want: string(DefaultTermColors().MaprTable.HeaderAttr),
			wantWarn: `invalid text attribute "Strike", using the default instead`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings := captureConfigWarnings(t)
			configPath := filepath.Join(t.TempDir(), "dtail.json")
			writeTestConfig(t, configPath, tt.body)
			cfg, err := SetupRuntime(source.Client, &Args{ConfigFile: configPath, SSHArgs: SSHArgs{NoAuthKey: true, SSHPort: DefaultSSHPort}}, nil)
			if err != nil {
				t.Fatalf("SetupRuntime failed on an invalid colour value: %v", err)
			}
			if got := tt.get(cfg.Client.TermColors); got != tt.want {
				t.Fatalf("value = %q, want default %q", got, tt.want)
			}
			out := warnings.String()
			for _, want := range []string{"WARN: config file " + configPath + ": ", tt.wantWarn, "case-insensitive"} {
				if !strings.Contains(out, want) {
					t.Fatalf("warning output %q does not contain %q", out, want)
				}
			}
			if strings.Count(out, "\n") != 1 {
				t.Fatalf("want exactly one warning line, got %q", out)
			}
		})
	}
}

// TestParseConfigAcceptsLegacyValues checks the values older releases ran
// with: "" for a colour (no escape code) and raw escape strings, including
// concatenated sequences and ':'-separated SGR parameters. None of them warns.
func TestParseConfigAcceptsLegacyValues(t *testing.T) {
	warnings := captureConfigWarnings(t)
	in, err := parseTestConfig(t, `{"Client": {"TermColors": {
  "Server": {"TextFg": "", "TextBg": "", "TextAttr": ""},
  "Remote": {"TextFg": "\u001b[31m\u001b[1m", "TextBg": "\u001b[48:5:17m", "TextAttr": "\u001b[1m\u001b[4m"}
}}}`)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	server, remote := in.Client.TermColors.Server, in.Client.TermColors.Remote
	if server.TextFg != "" || server.TextBg != "" || server.TextAttr != color.AttrNone {
		t.Fatalf("empty values = %q %q %q, want no escape codes", server.TextFg, server.TextBg, server.TextAttr)
	}
	if remote.TextFg != "\x1b[31m\x1b[1m" || remote.TextBg != "\x1b[48:5:17m" ||
		remote.TextAttr != "\x1b[1m\x1b[4m" {
		t.Fatalf("raw escape values = %q %q %q", remote.TextFg, remote.TextBg, remote.TextAttr)
	}
	if warnings.Len() != 0 {
		t.Fatalf("unexpected warnings: %q", warnings.String())
	}
}

// TestReleasedExampleConfigLoads decodes the example config shipped with
// v4.2.0 through v4.3.4 (testdata copy of `git show v4.3.4:examples/dtail.json.example`),
// which doc/installation.md told users to install as /etc/dserver/dtail.json.
// Its prefixed names ("AttrDim", "BgCyan", "FgBlack") must load without a
// warning and yield the palette they name, which is the built-in default.
func TestReleasedExampleConfigLoads(t *testing.T) {
	warnings := captureConfigWarnings(t)
	path := filepath.Join("testdata", "dtail.json.v4.3.4.example")
	for _, sourceProcess := range []source.Source{source.Client, source.Server} {
		t.Run(sourceProcess.String(), func(t *testing.T) {
			cfg, err := SetupRuntime(sourceProcess, &Args{ConfigFile: path, SSHArgs: SSHArgs{NoAuthKey: true, SSHPort: DefaultSSHPort}}, nil)
			if err != nil {
				t.Fatalf("SetupRuntime: %v", err)
			}
			got := cfg.Client.TermColors
			if got.Server.DelimiterAttr != color.AttrDim || got.Server.ServerBg != color.BgCyan ||
				got.Server.HostnameFg != color.FgBlack || got.Server.HostnameAttr != color.AttrBold ||
				got.Common.SeverityFatalBg != color.BgMagenta || got.MaprTable.HeaderSortKeyAttr != color.AttrUnderline ||
				got.MaprTable.HeaderGroupKeyAttr != color.AttrReverse || got.MaprTable.RawQueryFg != color.FgCyan ||
				got.MaprTable.DataAttr != color.AttrNone {
				t.Fatalf("prefixed names decoded wrongly: %+v", got)
			}
			if want := DefaultTermColors(); got != want {
				t.Fatalf("TermColors = %+v, want %+v", got, want)
			}
		})
	}
	if warnings.Len() != 0 {
		t.Fatalf("unexpected warnings: %q", warnings.String())
	}
}

// TestShippedConfigsDecodeToDefaultPalette checks that the palettes shipped in
// the example and docker configs decode to escape codes; both spell out the
// built-in default palette (or omit it), so they must equal the defaults.
func TestShippedConfigsDecodeToDefaultPalette(t *testing.T) {
	for _, path := range []string{"../../examples/dtail.json.example", "../../docker/dtail.json"} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			in := initializer{
				Common: newDefaultCommonConfig(),
				Server: newDefaultServerConfig(),
				Client: newDefaultClientConfig(),
			}
			if err := in.parseConfig(&Args{ConfigFile: path}); err != nil {
				t.Fatalf("parseConfig: %v", err)
			}
			if got, want := in.Client.TermColors, DefaultTermColors(); got != want {
				t.Fatalf("TermColors = %+v, want defaults %+v", got, want)
			}
		})
	}
}

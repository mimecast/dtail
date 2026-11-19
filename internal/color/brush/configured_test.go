package brush

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/source"
)

// TestColorfyWithConfiguredPalette loads a palette of colour names from a
// config file and checks that a remote line is painted with the matching
// escape codes rather than with the literal names.
func TestColorfyWithConfiguredPalette(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "dtail.json")
	body := `{"Client": {"TermColors": {"Remote": {"TextFg": "Red", "TextBg": "black", "TextAttr": "Bold"}}}}`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.SetupRuntime(source.Client,
		&config.Args{ConfigFile: configPath, SSHArgs: config.SSHArgs{NoAuthKey: true, SSHPort: config.DefaultSSHPort}}, nil)
	if err != nil {
		t.Fatalf("SetupRuntime: %v", err)
	}

	got := New(cfg.Client.TermColors).Colorfy("REMOTE|host|100|1|app.log|hello world")

	want := string(color.FgRed) + string(color.BgBlack) + string(color.AttrBold) +
		"hello world" + string(color.AttrReset) + string(color.BgDefault) + string(color.FgDefault)
	if !strings.HasSuffix(got, want) {
		t.Fatalf("Colorfy() = %q, want suffix %q", got, want)
	}
	for _, name := range []string{"Red", "black", "Bold"} {
		if strings.Contains(got, name) {
			t.Fatalf("Colorfy() = %q contains the literal colour name %q", got, name)
		}
	}
	// Keys the config omits keep the built-in palette.
	defaults := New(config.DefaultTermColors()).Colorfy("REMOTE|host|100|1|app.log|hello world")
	prefix := strings.TrimSuffix(got, want)
	if !strings.HasPrefix(defaults, prefix) {
		t.Fatalf("configured prefix %q differs from the default rendering %q", prefix, defaults)
	}
}

package cli

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/source"
)

// TestClientLoggingFlagPrecedence parses the shared client flags and runs the
// real config setup: an explicit --logger/--logDir wins over the config file,
// the config file wins over the client default, and the client default applies
// when neither is set.
func TestClientLoggingFlagPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(t.TempDir(), "dtail.json")
	if err := os.WriteFile(configPath,
		[]byte(`{"Common":{"Logger":"none","LogDir":"/cfg/logs"}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name       string
		argv       []string
		wantLogger string
		wantLogDir string
	}{
		{"default", []string{"-cfg", "none"}, config.DefaultClientLogger, home + "/log"},
		{"config", []string{"-cfg", configPath}, "none", "/cfg/logs"},
		{"flag", []string{"-cfg", configPath, "-logger", "stdout", "-logDir", "/flag/logs"},
			"stdout", "/flag/logs"},
		{"flag equal to default", []string{"-cfg", configPath, "-logger", config.DefaultClientLogger},
			config.DefaultClientLogger, "/cfg/logs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			var args config.Args
			BindCommonClientFlags(fs, &args)
			if err := fs.Parse(append([]string{"-no-auth-key"}, tt.argv...)); err != nil {
				t.Fatalf("Parse: %v", err)
			}
			cfg, err := config.SetupRuntime(source.Client, &args, fs.Args())
			if err != nil {
				t.Fatalf("SetupRuntime: %v", err)
			}
			if cfg.Common.Logger != tt.wantLogger {
				t.Errorf("Common.Logger = %q, want %q", cfg.Common.Logger, tt.wantLogger)
			}
			if cfg.Common.LogDir != tt.wantLogDir {
				t.Errorf("Common.LogDir = %q, want %q", cfg.Common.LogDir, tt.wantLogDir)
			}
		})
	}
}

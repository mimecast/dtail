package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/source"
)

// TestDServerLoggingFlagPrecedence parses the dserver flags and runs the real
// config setup: an explicit --logger/--logDir wins over the config file, the
// config file wins over the dserver default, and the dserver default applies
// when neither is set.
func TestDServerLoggingFlagPrecedence(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "dtail.json")
	if err := os.WriteFile(configPath,
		[]byte(`{"Common":{"Logger":"stdout","LogDir":"/cfg/logs"}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	emptyConfigPath := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(emptyConfigPath, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name       string
		argv       []string
		wantLogger string
		wantLogDir string
	}{
		{"default", []string{"-cfg", emptyConfigPath}, config.DefaultServerLogger, config.DefaultServerLogDir},
		{"config", []string{"-cfg", configPath}, "stdout", "/cfg/logs"},
		{"flag", []string{"-cfg", configPath, "-logger", "none", "-logDir", "/flag/logs"},
			"none", "/flag/logs"},
		{"flag equal to default", []string{"-cfg", configPath, "-logger", config.DefaultServerLogger},
			config.DefaultServerLogger, "/cfg/logs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			flags := bindDServerFlags(fs)
			if err := fs.Parse(tt.argv); err != nil {
				t.Fatalf("Parse: %v", err)
			}
			cfg, err := config.SetupRuntime(source.Server, &flags.args, fs.Args())
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

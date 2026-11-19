package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/source"
)

// TestEditMode checks Client.EditMode: --edit-mode wins over the config
// file, which wins over the default emacs; an empty value is emacs and an
// unknown one fails the client's config.
func TestEditMode(t *testing.T) {
	tests := []struct {
		name, file, flag, want string
		fails                  bool
	}{
		{name: "default", want: EditModeEmacs},
		{name: "config file", file: `{"Client": {"EditMode": "vi"}}`, want: EditModeVi},
		{name: "flag wins", file: `{"Client": {"EditMode": "vi"}}`, flag: "emacs", want: EditModeEmacs},
		{name: "flag only", flag: "vi", want: EditModeVi},
		{name: "empty in file", file: `{"Client": {"EditMode": ""}}`, want: EditModeEmacs},
		{name: "unknown", flag: "vim", fails: true},
		{name: "unknown in file", file: `{"Client": {"EditMode": "Vi"}}`, fails: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := initializer{Common: newDefaultCommonConfig(), Server: newDefaultServerConfig(),
				Client: newDefaultClientConfig()}
			args := &Args{ConfigFile: "none"}
			args.SSHPort = DefaultSSHPort
			args.LogLevel = DefaultLogLevel
			args.EditMode = tt.flag
			if tt.file != "" {
				path := filepath.Join(t.TempDir(), "dtail.json")
				if err := os.WriteFile(path, []byte(tt.file), 0o600); err != nil {
					t.Fatal(err)
				}
				args.ConfigFile = path
			}
			if err := in.parseConfig(args); err != nil {
				t.Fatal(err)
			}
			err := in.transformConfig(source.Client, args, nil)
			if tt.fails {
				if err == nil || !strings.Contains(err.Error(), "edit mode") {
					t.Fatalf("edit mode %q: error %v, want an edit mode error", in.Client.EditMode, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if in.Client.EditMode != tt.want {
				t.Fatalf("EditMode = %q, want %q", in.Client.EditMode, tt.want)
			}
		})
	}
}

// TestEditModeOnlyChecksClients checks that a bad Client.EditMode in a
// shared config file stops neither dserver nor dtailhealth.
func TestEditModeOnlyChecksClients(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dtail.json")
	if err := os.WriteFile(path, []byte(`{"Client": {"EditMode": "Vi"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, src := range []source.Source{source.Server, source.HealthCheck} {
		in := initializer{Common: newDefaultCommonConfig(), Server: newDefaultServerConfig(),
			Client: newDefaultClientConfig()}
		args := &Args{ConfigFile: path}
		args.SSHPort = DefaultSSHPort
		args.LogLevel = DefaultLogLevel
		if err := in.parseConfig(args); err != nil {
			t.Fatal(err)
		}
		if err := in.transformConfig(src, args, nil); err != nil {
			t.Errorf("%v: %v", src, err)
		}
	}
}

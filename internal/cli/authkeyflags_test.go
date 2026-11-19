package cli

import (
	"flag"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

func TestApplyAuthKeyPathCompatibilityLegacyOnly(t *testing.T) {
	args := config.Args{}

	if warning := ApplyAuthKeyPathCompatibility(&args, "/tmp/legacy.pem", false); warning != "" {
		t.Fatalf("unexpected warning: %q", warning)
	}

	if got, want := args.SSHPrivateKeyFilePath, "/tmp/legacy.pem"; got != want {
		t.Fatalf("unexpected auth key path: want %q got %q", want, got)
	}
}

func TestApplyAuthKeyPathCompatibilityPrefersExplicitAuthKeyPath(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
		warn bool
	}{
		{
			name: "legacy then auth",
			args: []string{"-key", "/tmp/legacy.pem", "-auth-key-path", "/tmp/current.pem"},
			want: "/tmp/current.pem",
			warn: true,
		},
		{
			name: "auth then legacy",
			args: []string{"-auth-key-path", "/tmp/current.pem", "-key", "/tmp/legacy.pem"},
			want: "/tmp/current.pem",
			warn: true,
		},
		{
			name: "explicit blank auth key path keeps blank",
			args: []string{"-key", "/tmp/legacy.pem", "-auth-key-path="},
			want: "",
			warn: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
			var args config.Args
			var legacyKey string

			BindAuthKeyFlags(fs, &legacyKey, &args)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("Parse failed: %v", err)
			}

			var authKeyPathSet bool
			fs.Visit(func(f *flag.Flag) {
				if f.Name == "auth-key-path" {
					authKeyPathSet = true
				}
			})

			warning := ApplyAuthKeyPathCompatibility(&args, legacyKey, authKeyPathSet)
			if gotWarn := warning != ""; gotWarn != tc.warn {
				t.Fatalf("unexpected warning presence: want %v got %v (%q)", tc.warn, gotWarn, warning)
			}
			if got, want := args.SSHPrivateKeyFilePath, tc.want; got != want {
				t.Fatalf("unexpected auth key path: want %q got %q", want, got)
			}
		})
	}
}

func TestBindAuthKeyFlagsHelpText(t *testing.T) {
	fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
	var args config.Args
	var legacyKey string

	BindAuthKeyFlags(fs, &legacyKey, &args)

	if got, want := fs.Lookup("key").Usage, "Deprecated alias for -auth-key-path"; got != want {
		t.Fatalf("unexpected legacy flag help: want %q got %q", want, got)
	}
	if got, want := fs.Lookup("auth-key-path").Usage, authKeyPathHelpText; got != want {
		t.Fatalf("unexpected auth-key-path help: want %q got %q", want, got)
	}
	if got, want := fs.Lookup("auth-key-path").DefValue, ""; got != want {
		t.Fatalf("unexpected auth-key-path default: want %q got %q", want, got)
	}
}

func TestFlagWasSetUsesProvidedFlagSet(t *testing.T) {
	fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
	var value string
	fs.StringVar(&value, "value", "", "test value")
	if err := fs.Parse([]string{"-value", "set"}); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if !FlagWasSet(fs, "value") {
		t.Fatal("FlagWasSet returned false for an explicitly set flag")
	}
	if FlagWasSet(fs, "missing") {
		t.Fatal("FlagWasSet returned true for a missing flag")
	}
}

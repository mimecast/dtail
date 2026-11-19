package clients

import (
	"testing"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

func TestSessionSpecCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    SessionSpec
		want    []string
		wantErr bool
	}{
		{
			name: "tail read commands",
			spec: SessionSpec{
				Mode:    omode.TailClient,
				Files:   []string{"/var/log/app.log"},
				Options: "plain=true",
				Regex:   "ERROR",
			},
			want: []string{"tail:plain=true /var/log/app.log regex:default,literal ERROR"},
		},
		{
			name: "map client commands",
			spec: SessionSpec{
				Mode:    omode.MapClient,
				Files:   []string{"/var/log/app.log"},
				Options: "plain=true",
				Query:   "from STATS select count(*)",
				Regex:   ".",
			},
			want: []string{
				"map:plain=true from STATS select count(*)",
				"cat:plain=true /var/log/app.log regex:noop ",
			},
		},
		{
			name: "tail query with timeout",
			spec: SessionSpec{
				Mode:    omode.TailClient,
				Files:   []string{"/var/log/app.log"},
				Options: "plain=true",
				Query:   "from STATS select count(*)",
				Regex:   "WARN",
				Timeout: 15,
			},
			want: []string{
				"map:plain=true from STATS select count(*)",
				"timeout 15 tail /var/log/app.log regex:default,literal WARN",
			},
		},
		{
			name: "health command",
			spec: SessionSpec{
				Mode: omode.HealthClient,
			},
			want: []string{"health"},
		},
		{
			name: "invalid regex returns error",
			spec: SessionSpec{
				Mode:  omode.GrepClient,
				Files: []string{"/var/log/app.log"},
				Regex: "[",
			},
			wantErr: true,
		},
		{
			name: "query with unsupported mode returns error",
			spec: SessionSpec{
				Mode:  omode.GrepClient,
				Files: []string{"/var/log/app.log"},
				Query: "from STATS select count(*)",
				Regex: ".",
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tc.spec.Commands()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Commands() error = %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("unexpected command count: got %d want %d (%v)", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("command %d mismatch: got %q want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestNewSessionSpecSplitsFiles(t *testing.T) {
	t.Parallel()

	spec := NewSessionSpec(config.Args{
		Mode:        omode.GrepClient,
		What:        " a.log , , b.log ",
		RegexStr:    "ERROR",
		RegexInvert: true,
		Timeout:     10,
	})

	if len(spec.Files) != 2 || spec.Files[0] != "a.log" || spec.Files[1] != "b.log" {
		t.Fatalf("unexpected files: %#v", spec.Files)
	}
	if !spec.RegexInvert {
		t.Fatalf("expected RegexInvert to be true")
	}
	if spec.Timeout != 10 {
		t.Fatalf("unexpected timeout: %d", spec.Timeout)
	}
}

func TestNewSessionSpecUsesPipeSentinelForServerlessStdin(t *testing.T) {
	t.Parallel()

	spec := NewSessionSpec(config.Args{
		Mode:       omode.GrepClient,
		Serverless: true,
		RegexStr:   "ERROR",
	})

	if len(spec.Files) != 1 || spec.Files[0] != "-" {
		t.Fatalf("unexpected files for serverless stdin: %#v", spec.Files)
	}
}

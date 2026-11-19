package session

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/omode"
)

func TestNewSpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		parameters Parameters
		want       Spec
	}{
		{
			name: "normalizes files and query",
			parameters: Parameters{
				Mode:              omode.GrepClient,
				What:              " app.log, , audit.log ",
				SerializedOptions: "plain=true",
				Query:             "  from STATS select count(*)  ",
				Regex:             "ERROR",
				RegexInvert:       true,
				Timeout:           12,
			},
			want: Spec{
				Mode:        omode.GrepClient,
				Files:       []string{"app.log", "audit.log"},
				Options:     "plain=true",
				Query:       "from STATS select count(*)",
				Regex:       "ERROR",
				RegexInvert: true,
				Timeout:     12,
			},
		},
		{
			name:       "serverless reader uses standard input",
			parameters: Parameters{Mode: omode.CatClient, Serverless: true, SerializedOptions: "serverless=true"},
			want:       Spec{Mode: omode.CatClient, Files: []string{"-"}, Options: "serverless=true"},
		},
		{
			name:       "health mode does not invent standard input",
			parameters: Parameters{Mode: omode.HealthClient, Serverless: true, SerializedOptions: "serverless=true"},
			want:       Spec{Mode: omode.HealthClient, Options: "serverless=true"},
		},
		{
			name:       "blank input stays empty in remote mode",
			parameters: Parameters{Mode: omode.TailClient, What: " ,  , "},
			want:       Spec{Mode: omode.TailClient, Files: []string{}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := NewSpec(tt.parameters); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("NewSpec() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestSpecCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		spec       Spec
		want       []string
		wantErrSub string
	}{
		{
			name: "health",
			spec: Spec{Mode: omode.HealthClient},
			want: []string{"health"},
		},
		{
			name: "read every file with inverted literal regex",
			spec: Spec{
				Mode:        omode.GrepClient,
				Files:       []string{"app.log", "audit.log"},
				Options:     "plain=true",
				Regex:       "DEBUG",
				RegexInvert: true,
			},
			want: []string{
				"grep:plain=true app.log regex:invert,literal DEBUG",
				"grep:plain=true audit.log regex:invert,literal DEBUG",
			},
		},
		{
			name: "map snapshot uses cat reads",
			spec: Spec{
				Mode:    omode.MapClient,
				Files:   []string{"stats.log"},
				Options: "plain=true",
				Query:   "from STATS select count(*)",
				Regex:   ".",
			},
			want: []string{
				"map:plain=true from STATS select count(*)",
				"cat:plain=true stats.log regex:noop ",
			},
		},
		{
			name: "tail query applies timeout",
			spec: Spec{
				Mode:    omode.TailClient,
				Files:   []string{"stats.log"},
				Options: "plain=true",
				Query:   "from STATS select count(*)",
				Regex:   "WARN",
				Timeout: 9,
			},
			want: []string{
				"map:plain=true from STATS select count(*)",
				"timeout 9 tail stats.log regex:default,literal WARN",
			},
		},
		{
			name:       "query rejects read-only mode",
			spec:       Spec{Mode: omode.GrepClient, Query: "select count(*)"},
			wantErrSub: "query mode requires map or tail mode",
		},
		{
			name:       "read rejects unsupported mode",
			spec:       Spec{Mode: omode.MapClient},
			wantErrSub: "unsupported session mode map",
		},
		{
			name:       "read reports invalid regex",
			spec:       Spec{Mode: omode.CatClient, Files: []string{"app.log"}, Regex: "["},
			wantErrSub: "missing closing ]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.spec.Commands()
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("Commands() error = %v, want substring %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("Commands() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Commands() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestSpecStartCommandEncodesPayload(t *testing.T) {
	t.Parallel()

	spec := Spec{
		Mode:    omode.TailClient,
		Files:   []string{"/var/log/app.log"},
		Options: "plain=true",
		Regex:   "ERROR",
		Timeout: 15,
	}

	command, err := spec.StartCommand()
	if err != nil {
		t.Fatalf("StartCommand() error = %v", err)
	}
	if !strings.HasPrefix(command, "SESSION START ") {
		t.Fatalf("unexpected start command prefix: %q", command)
	}

	var decoded Spec
	if err := decodeSpecPayload(strings.TrimPrefix(command, "SESSION START "), &decoded); err != nil {
		t.Fatalf("decode start payload: %v", err)
	}
	if !reflect.DeepEqual(decoded, spec) {
		t.Fatalf("unexpected decoded spec: got %#v want %#v", decoded, spec)
	}
}

func TestSpecUpdateCommandIncludesGeneration(t *testing.T) {
	t.Parallel()

	spec := Spec{
		Mode:  omode.MapClient,
		Files: []string{"/var/log/app.log"},
		Query: "from STATS select count(*)",
	}

	command, err := spec.UpdateCommand(7)
	if err != nil {
		t.Fatalf("UpdateCommand() error = %v", err)
	}
	if !strings.HasPrefix(command, "SESSION UPDATE 7 ") {
		t.Fatalf("unexpected update command prefix: %q", command)
	}

	var decoded Spec
	if err := decodeSpecPayload(strings.TrimPrefix(command, "SESSION UPDATE 7 "), &decoded); err != nil {
		t.Fatalf("decode update payload: %v", err)
	}
	if !reflect.DeepEqual(decoded, spec) {
		t.Fatalf("unexpected decoded spec: got %#v want %#v", decoded, spec)
	}
}

func TestSpecUpdateCommandWithoutGeneration(t *testing.T) {
	t.Parallel()

	spec := Spec{Mode: omode.CatClient, Files: []string{"app.log"}}
	command, err := spec.UpdateCommand(0)
	if err != nil {
		t.Fatalf("UpdateCommand() error = %v", err)
	}
	if !strings.HasPrefix(command, "SESSION UPDATE ") {
		t.Fatalf("unexpected update command prefix: %q", command)
	}

	var decoded Spec
	if err := decodeSpecPayload(strings.TrimPrefix(command, "SESSION UPDATE "), &decoded); err != nil {
		t.Fatalf("decode update payload: %v", err)
	}
	if !reflect.DeepEqual(decoded, spec) {
		t.Fatalf("unexpected decoded spec: got %#v want %#v", decoded, spec)
	}
}

func TestSpecHasJournalFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files []string
		want  bool
	}{
		{
			name:  "journal file",
			files: []string{"journal:ssh.service"},
			want:  true,
		},
		{
			name:  "journal file with surrounding spaces",
			files: []string{" /var/log/app.log ", " journal:nginx.service "},
			want:  true,
		},
		{
			name:  "regular file",
			files: []string{"/var/log/app.log"},
			want:  false,
		},
		{
			name:  "journal substring is not prefix",
			files: []string{"/var/log/journal:ssh.service.log"},
			want:  false,
		},
		{
			name: "empty files",
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := Spec{Files: tc.files}
			if got := spec.HasJournalFiles(); got != tc.want {
				t.Fatalf("HasJournalFiles() = %v, want %v", got, tc.want)
			}
		})
	}
}

func decodeSpecPayload(payload string, out *Spec) error {
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

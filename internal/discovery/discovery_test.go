package discovery

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

var testLogger logging.NopLogger

// TestShuffleListDoesNotMutateInput verifies that shuffleList never writes
// back into the caller's backing array, and that repeated calls within the
// same sub-second window produce different orderings (the chance of a false
// failure with 20 elements is 1/20! ≈ 4×10⁻¹⁹).
func TestShuffleListDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	d := &Discovery{logger: testLogger}

	// Build a stable reference slice.
	original := make([]string, 20)
	for i := range original {
		original[i] = string(rune('a' + i))
	}

	snapshot := append([]string(nil), original...)

	// First shuffle — must not touch original.
	first := d.shuffleList(original)
	if !reflect.DeepEqual(original, snapshot) {
		t.Fatalf("shuffleList mutated caller's slice: got %v, want %v", original, snapshot)
	}
	if len(first) != len(original) {
		t.Fatalf("shuffleList returned wrong length: got %d, want %d", len(first), len(original))
	}

	// Second shuffle — different seed path; overwhelmingly likely to differ.
	second := d.shuffleList(original)
	if reflect.DeepEqual(first, second) {
		t.Fatal("shuffleList returned identical order on two consecutive calls; seed resolution may be too coarse")
	}

	// Both results must contain exactly the same elements as the input.
	countFirst := make(map[string]int, len(original))
	countSecond := make(map[string]int, len(original))
	for i := range original {
		countFirst[first[i]]++
		countSecond[second[i]]++
	}
	for _, s := range original {
		if countFirst[s] != 1 {
			t.Errorf("element %q appears %d times in first shuffle, want 1", s, countFirst[s])
		}
		if countSecond[s] != 1 {
			t.Errorf("element %q appears %d times in second shuffle, want 1", s, countSecond[s])
		}
	}
}

func TestNewParsesModuleOptionsWithAdditionalColons(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		method  string
		wantMod string
		wantOpt string
		wantErr bool
	}{
		{
			name:    "plain module",
			method:  "file",
			wantMod: "FILE",
		},
		{
			name:    "options with additional colons",
			method:  "file:host:port:extra",
			wantMod: "FILE",
			wantOpt: "host:port:extra",
		},
		{
			name:    "empty options rejected",
			method:  "method:",
			wantErr: true,
		},
		{
			name:    "missing module rejected",
			method:  ":host:port",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := New(tt.method, "server", Shuffle, testLogger)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("New(%q) error = nil, want error", tt.method)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%q) error = %v, want nil", tt.method, err)
			}
			if got.module != tt.wantMod {
				t.Fatalf("module = %q, want %q", got.module, tt.wantMod)
			}
			if got.options != tt.wantOpt {
				t.Fatalf("options = %q, want %q", got.options, tt.wantOpt)
			}
		})
	}
}

func TestServerListIgnoresEmptyServerEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		method  string
		server  string
		want    []string
		wantErr bool
	}{
		{
			name:   "regex without module yields no phantom host",
			server: "/.*/",
			want:   []string{},
		},
		{
			name:   "comma list filters empty entries",
			server: "alpha,,beta,",
			want:   []string{"alpha", "beta"},
		},
		{
			name:   "empty server input preserves serverless sentinel",
			server: "",
			want:   []string{""},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := New(tt.method, tt.server, ServerOrder(99), testLogger)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("New(%q, %q) error = nil, want error", tt.method, tt.server)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%q, %q) error = %v, want nil", tt.method, tt.server, err)
			}

			servers, err := got.ServerList()
			if err != nil {
				t.Fatalf("ServerList() error = %v", err)
			}
			if len(servers) != len(tt.want) {
				t.Fatalf("ServerList() len = %d, want %d (%v)", len(servers), len(tt.want), servers)
			}
			for i := range tt.want {
				if servers[i] != tt.want[i] {
					t.Fatalf("ServerList()[%d] = %q, want %q (full=%v)", i, servers[i], tt.want[i], servers)
				}
			}
		})
	}
}

func TestNewReturnsDiscoveryInputErrors(t *testing.T) {
	tests := []struct {
		name   string
		method string
		server string
		want   string
	}{
		{name: "invalid regex", server: "/[/", want: "compile server discovery regex"},
		{name: "unknown module", method: "bogus", server: "host", want: "unknown server discovery module"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.method, tt.server, Shuffle, testLogger)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New(%q, %q) error = %v, want %q", tt.method, tt.server, err, tt.want)
			}
		})
	}
}

func TestServerListReturnsFileReadError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	discovery, err := New("file", missing, Shuffle, testLogger)
	if err != nil {
		t.Fatalf("New file discovery: %v", err)
	}

	_, err = discovery.ServerList()
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("ServerList error = %v, want missing file path", err)
	}
}

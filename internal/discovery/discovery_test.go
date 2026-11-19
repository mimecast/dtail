package discovery

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
			method:  "testopts:host:port:extra",
			wantMod: "TESTOPTS",
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
			name:   "regex with surrounding whitespace yields no phantom host",
			server: " /web/ ",
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
		{
			name:   "comma list trims whitespace around entries",
			server: "alpha:2222, beta:2222 ,\tgamma ",
			want:   []string{"alpha:2222", "beta:2222", "gamma"},
		},
		{
			name:   "comma list dedups entries differing only in whitespace",
			server: "alpha, alpha,alpha ",
			want:   []string{"alpha"},
		},
		{
			name:   "whitespace-only comma list is no serverless sentinel",
			server: " , ,",
			want:   []string{},
		},
		{
			name:   "explicit comma module trims entries",
			method: "comma",
			server: " alpha , beta",
			want:   []string{"alpha", "beta"},
		},
		{
			name:   "explicit comma module keeps the empty-input sentinel",
			method: "comma",
			server: "",
			want:   []string{""},
		},
		{
			name:   "explicit comma module whitespace-only input is empty",
			method: "comma",
			server: " , ",
			want:   []string{},
		},
		// Trimming only removes surrounding whitespace: bad entries still
		// reach the client's address parser unchanged and fail there.
		{
			name:   "invalid port reaches the parser",
			server: " host:notaport ",
			want:   []string{"host:notaport"},
		},
		{
			name:   "inner whitespace is kept",
			server: "alpha beta, gamma",
			want:   []string{"alpha beta", "gamma"},
		},
	}

	for _, tt := range tests {
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

// TestNewTrimsARegexServerValue checks that whitespace around a /regex/
// server value is ignored like around every server entry: the value is the
// filter regex and not a host named "/web/".
func TestNewTrimsARegexServerValue(t *testing.T) {
	t.Parallel()

	for _, server := range []string{"/web/", " /web/ ", "\t/web/\n"} {
		d, err := New("", server, Shuffle, testLogger)
		if err != nil {
			t.Fatalf("New(%q) error = %v", server, err)
		}
		if d.regex == nil || d.regex.String() != "web" {
			t.Fatalf("New(%q) regex = %v, want web", server, d.regex)
		}
		if d.server != "" {
			t.Fatalf("New(%q) server = %q, want it cleared", server, d.server)
		}
		filtered := d.filterList([]string{"web1", "db1", "web2"})
		if !slices.Equal(filtered, []string{"web1", "web2"}) {
			t.Fatalf("New(%q) filter = %v, want [web1 web2]", server, filtered)
		}
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
		{name: "invalid regex with surrounding whitespace", server: " /[/\t", want: "compile server discovery regex"},
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

// TestServerListFromFileSkipsBlankAndCommentLines covers server list files
// with blank lines, indented entries, CRLF line endings, comments and a
// leading UTF-8 byte order mark. Each of these used to yield an empty,
// whitespace-wrapped or otherwise invalid address that aborted the whole
// client before it connected to any server.
func TestServerListFromFileSkipsBlankAndCommentLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		method  string
		content string
		want    []string
	}{
		{name: "trailing blank line", content: "alpha:2222\n\n", want: []string{"alpha:2222"}},
		{name: "blank separator lines", content: "alpha\n\n \t\nbeta\n", want: []string{"alpha", "beta"}},
		{name: "indented and padded entries", content: "  alpha  \n\tbeta\t\n", want: []string{"alpha", "beta"}},
		{name: "crlf line endings", content: "alpha\r\nbeta\r\n\r\n", want: []string{"alpha", "beta"}},
		{name: "comment lines", content: "# servers\nalpha\n  # beta\ngamma\n", want: []string{"alpha", "gamma"}},
		{name: "dedup after trimming", content: "alpha\n alpha\nalpha \n", want: []string{"alpha"}},
		{name: "only blank and comment lines", content: "\n# none\n  \n", want: []string{}},
		{name: "explicit file module", method: "file", content: "\nalpha\n", want: []string{"alpha"}},
		// Editors may write a UTF-8 byte order mark, which TrimSpace keeps.
		{name: "utf-8 bom", content: "\uFEFFalpha\nbeta\n", want: []string{"alpha", "beta"}},
		{name: "utf-8 bom before a comment", content: "\uFEFF# servers\nalpha\n", want: []string{"alpha"}},
		{name: "utf-8 bom before padding", content: "\uFEFF  alpha\n", want: []string{"alpha"}},
		{name: "bom only stripped at the start", content: "alpha\n\uFEFFbeta\n",
			want: []string{"alpha", "\uFEFFbeta"}},
		// A '#' starts a comment only at the start of a line: an inline
		// comment is part of the entry and fails in the address parser.
		{name: "inline comment is no comment", content: "host # c\n", want: []string{"host # c"}},
		{name: "invalid port reaches the parser", content: "host:notaport\n", want: []string{"host:notaport"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "servers.txt")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write server list file: %v", err)
			}
			finder, err := New(tt.method, path, ServerOrder(99), testLogger)
			if err != nil {
				t.Fatalf("New(%q, %q) error = %v", tt.method, path, err)
			}
			servers, err := finder.ServerList()
			if err != nil {
				t.Fatalf("ServerList() error = %v", err)
			}
			if len(servers) != len(tt.want) || (len(servers) > 0 && !reflect.DeepEqual(servers, tt.want)) {
				t.Fatalf("ServerList() = %q, want %q", servers, tt.want)
			}
		})
	}
}

// TestServerListIgnoresWhitespaceAroundTheFilePath checks that a server
// value naming a server list file with surrounding whitespace is read as that
// file, without and with the explicit file module, and not dialled as the
// host "servers.txt ".
func TestServerListIgnoresWhitespaceAroundTheFilePath(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "servers.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatalf("write server list file: %v", err)
	}
	for _, method := range []string{"", "file"} {
		for _, server := range []string{path + " ", "\t" + path + "\n"} {
			if method == "" && !IsServerListFile(server) {
				t.Fatalf("IsServerListFile(%q) = false, want true", server)
			}
			finder, err := New(method, server, ServerOrder(99), testLogger)
			if err != nil {
				t.Fatalf("New(%q, %q) error = %v", method, server, err)
			}
			servers, err := finder.ServerList()
			if err != nil {
				t.Fatalf("ServerList(%q, %q) error = %v", method, server, err)
			}
			if want := []string{"alpha", "beta"}; !reflect.DeepEqual(servers, want) {
				t.Fatalf("ServerList(%q, %q) = %q, want %q", method, server, servers, want)
			}
		}
	}
	if IsServerListFile(path + ".missing") {
		t.Fatal("IsServerListFile of a missing file = true, want false")
	}
}

// ServerListFromTESTOPTS is a test-only discovery module standing in for a
// module a build adds: it reads its servers from its options, which the
// built-in COMMA and FILE modules reject.
func (d *Discovery) ServerListFromTESTOPTS() ([]string, error) {
	return strings.Split(d.options, ","), nil
}

// TestBuiltInModulesRejectOptions checks that "MODULE:options" is rejected
// for the built-in COMMA and FILE modules in any case, including names such
// as "fıle" (dotless ı) that strings.ToUpper maps to FILE although
// strings.EqualFold does not match them with "file". The options never
// carried the server list: "comma:server1,server2" with an empty server value
// used to query this dserver silently, so a scheduled job wrote a wrong
// outfile and was skipped for the rest of its period.
func TestBuiltInModulesRejectOptions(t *testing.T) {
	t.Parallel()

	for _, method := range []string{
		"comma:x", "COMMA:server1,server2", "Comma:a:b",
		"file:/p", "FILE:/path/to/serverlist", "File:x", "fıle:/p",
	} {
		for _, server := range []string{"", "alpha,beta"} {
			_, err := New(method, server, ServerOrder(99), testLogger)
			if err == nil || !strings.Contains(err.Error(), "takes no options") {
				t.Fatalf("New(%q, %q) error = %v, want takes no options", method, server, err)
			}
		}
	}
}

// TestModulesWithoutOptionsStillWork checks that COMMA and FILE without
// options, in any case, read the server value, and that a module a build adds
// can still read its options.
func TestModulesWithoutOptionsStillWork(t *testing.T) {
	t.Parallel()

	listPath := filepath.Join(t.TempDir(), "servers.txt")
	if err := os.WriteFile(listPath, []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatalf("write server list file: %v", err)
	}
	tests := []struct {
		method string
		server string
	}{
		{method: "comma", server: "alpha,beta"},
		{method: "COMMA", server: "alpha,beta"},
		{method: "Comma", server: "alpha,beta"},
		{method: "file", server: listPath},
		{method: "FILE", server: listPath},
		{method: "testopts:alpha,beta", server: "ignored"},
	}
	for _, tt := range tests {
		finder, err := New(tt.method, tt.server, ServerOrder(99), testLogger)
		if err != nil {
			t.Fatalf("New(%q, %q) error = %v", tt.method, tt.server, err)
		}
		servers, err := finder.ServerList()
		if err != nil {
			t.Fatalf("ServerList(%q) error = %v", tt.method, err)
		}
		if want := []string{"alpha", "beta"}; !reflect.DeepEqual(servers, want) {
			t.Fatalf("ServerList(%q) = %q, want %q", tt.method, servers, want)
		}
	}
}

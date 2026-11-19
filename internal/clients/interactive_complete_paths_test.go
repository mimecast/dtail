package clients

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/protocol"
)

// pathTestSource returns a completion source over a catalog with listings
// of /var/log/, the working directory and the journal units, as server
// srv1 sent them, for servers connections.
func pathTestSource(servers int) pathCompletionSource {
	now := time.Unix(1_000_000, 0)
	c := newFieldCatalog()
	entries := func(names ...string) []protocol.ProbeEntry {
		var out []protocol.ProbeEntry
		for _, name := range names {
			if dir, ok := strings.CutSuffix(name, "/"); ok {
				out = append(out, protocol.ProbeEntry{Name: dir, Dir: true})
				continue
			}
			out = append(out, protocol.ProbeEntry{Name: name})
		}
		return out
	}
	c.storeListing("srv1", "/var/log/", protocol.ProbeAnswer{Entries: entries("apache2/", "app.log", "app.log.1",
		"b,c.log", "bee.log", " lead.log", "it's.log", "my file.log", "x$y.log", "tab\tname.log",
		"be\u202egol.exe")}, now)
	c.storeListing("srv1", "./", protocol.ProbeAnswer{Entries: entries("local.log")}, now)
	c.storeListing("srv1", "journal:", protocol.ProbeAnswer{Entries: entries("ssh.service", "sshd.service")}, now)
	c.storeListing("srv1", "/off/", protocol.ProbeAnswer{Errors: []protocol.ProbeError{
		{Error: protocol.ProbeErrorListDisabled}}}, now)
	return pathCompletionSource{
		find:    func(dir, base string) listingMatch { return c.findListing(dir, base, now) },
		servers: servers,
	}
}

// TestCompleteFilesValue covers Tab inside a :reload --files value: the
// last comma-separated entry, directories, common prefixes, candidate
// lists, names that need quoting in every shell quote context, names that
// can not be read as an entry, journal units, uncached directories and
// the cases left to completeInteractive.
func TestCompleteFilesValue(t *testing.T) {
	tests := []struct {
		name string
		line string
		// after is the text after the cursor, which stays.
		after          string
		wantOK         bool
		wantLine       string
		wantFiles      string // the --files value the completed line parses to
		wantCandidates []string
		wantFetch      string
		wantHint       string
	}{
		{name: "directory", line: ":reload --files /var/log/apa", wantOK: true,
			wantLine: ":reload --files /var/log/apache2/", wantFiles: "/var/log/apache2/"},
		{name: "common prefix", line: ":reload --files /var/log/app", wantOK: true,
			wantLine: ":reload --files /var/log/app.log", wantFiles: "/var/log/app.log"},
		{name: "list", line: ":reload --files /var/log/app.log", wantOK: true,
			wantLine: ":reload --files /var/log/app.log", wantCandidates: []string{"app.log", "app.log.1"}},
		{name: "blank escaped outside quotes", line: ":reload --files /var/log/my", wantOK: true,
			wantLine: `:reload --files /var/log/my\ file.log`, wantFiles: "/var/log/my file.log"},
		{name: "blank in single quotes", line: ":reload --files '/var/log/my", wantOK: true,
			wantLine: `:reload --files '/var/log/my file.log`, wantFiles: "/var/log/my file.log"},
		{name: "dollar in double quotes", line: `:reload --files "/var/log/x`, wantOK: true,
			wantLine: `:reload --files "/var/log/x\$y.log`, wantFiles: "/var/log/x$y.log"},
		{name: "quote in single quotes", line: ":reload --files '/var/log/it", wantOK: true,
			wantLine: `:reload --files '/var/log/it'\''s.log`, wantFiles: "/var/log/it's.log"},
		{name: "equals form", line: ":reload --files=/var/log/apa", wantOK: true,
			wantLine: ":reload --files=/var/log/apache2/", wantFiles: "/var/log/apache2/"},
		{name: "single dash", line: ":reload -files /var/log/apa", wantOK: true,
			wantLine: ":reload -files /var/log/apache2/", wantFiles: "/var/log/apache2/"},
		{name: "last entry of a list", line: ":reload --files /var/log/app.log,/var/log/apa", after: " --regex x",
			wantOK: true, wantLine: ":reload --files /var/log/app.log,/var/log/apache2/ --regex x"},
		{name: "last entry after blanks", line: ":reload --files '/var/log/app.log,  /var/log/apa", after: "' --regex x",
			wantOK: true, wantLine: ":reload --files '/var/log/app.log,  /var/log/apache2/' --regex x"},
		{name: "entry after a comma", line: ":reload --files /var/log/app.log,lo", wantOK: true,
			wantLine: ":reload --files /var/log/app.log,local.log", wantFiles: "/var/log/app.log,local.log"},
		{name: "names with a comma, blanks around or a format character are not offered", line: ":reload --files /var/log/b",
			wantOK: true, wantLine: ":reload --files /var/log/bee.log", wantFiles: "/var/log/bee.log"},
		{name: "working directory", line: ":reload --files lo", wantOK: true,
			wantLine: ":reload --files local.log", wantFiles: "local.log"},
		{name: "journal units listed", line: ":reload --files journal:ssh", wantOK: true,
			wantLine: ":reload --files journal:ssh", wantCandidates: []string{"ssh.service", "sshd.service"}},
		{name: "journal unit", line: ":reload --files journal:sshd", wantOK: true,
			wantLine: ":reload --files journal:sshd.service", wantFiles: "journal:sshd.service"},
		{name: "uncached directory", line: ":reload --files /var/other/x", wantOK: true,
			wantLine: ":reload --files /var/other/x", wantFetch: "/var/other/"},
		{name: "listing disabled", line: ":reload --files /off/x", wantOK: true,
			wantLine: ":reload --files /off/x", wantHint: "(path listing: " + protocol.ProbeErrorListDisabled + ")"},
		{name: "base name across quotes is left alone", line: ":reload --files /var/log/'my f'il", wantOK: true,
			wantLine: ":reload --files /var/log/'my f'il"},
		{name: "value not started", line: ":reload --files ", wantOK: false},
		{name: "other flag", line: ":reload --regex /var/log/a", wantOK: false},
		{name: "flag word", line: ":reload --files --reg", wantOK: false},
		{name: "no reload", line: ":show /var/log/a", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := completeFilesValue(tt.line+tt.after, len(tt.line), completeGrepArgs, pathTestSource(1))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.line != tt.wantLine || got.pos != len(got.line)-len(tt.after) ||
				!reflect.DeepEqual(got.candidates, tt.wantCandidates) || got.fetch != tt.wantFetch ||
				got.hint != tt.wantHint || got.header != "" {
				t.Fatalf("got line %q pos %d candidates %q fetch %q hint %q header %q", got.line, got.pos,
					got.candidates, got.fetch, got.hint, got.header)
			}
			if tt.wantFiles != "" {
				requireReloadFiles(t, got.line, tt.wantFiles)
			}
		})
	}
}

// requireReloadFiles requires that line, a :reload, parses (with the
// strict tokenizer :reload runs, after closing a quote left open) to the
// --files value want.
func requireReloadFiles(t *testing.T, line, want string) {
	t.Helper()
	args := strings.TrimPrefix(line, ":reload")
	if quote := splitInteractiveArgsPartial(args).quote; quote != 0 {
		args += string(quote)
	}
	tokens, err := splitInteractiveArgs(args)
	if err != nil {
		t.Fatalf("completed line %q does not tokenize: %v", line, err)
	}
	value := tokens[len(tokens)-1]
	if name, v, ok := strings.Cut(value, "="); ok && strings.HasPrefix(name, "-") {
		value = v
	}
	if value != want {
		t.Fatalf("completed line %q parses to --files %q, want %q", line, value, want)
	}
}

// TestCompleteFilesValueSeveralServers: a candidate list says which server
// listed it when several are connected; with one there is no header.
func TestCompleteFilesValueSeveralServers(t *testing.T) {
	line := ":reload --files /var/log/app.log"
	got, _ := completeFilesValue(line, len(line), completeGrepArgs, pathTestSource(3))
	if got.header != "(files as listed by srv1, one of 3 servers)" || len(got.candidates) != 2 {
		t.Fatalf("header %q candidates %q", got.header, got.candidates)
	}
	line = ":reload --files /var/log/apa"
	if got, _ := completeFilesValue(line, len(line), completeGrepArgs, pathTestSource(3)); got.header != "" {
		t.Fatalf("header %q for a completed name", got.header)
	}
}

// TestCompleteFilesValueTruncated: a truncated listing still completes
// while the narrower listing is asked for, and a truncated listing of the
// typed name itself says that it was cut.
func TestCompleteFilesValueTruncated(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := newFieldCatalog()
	c.storeListing("srv1", "/big/", listingAnswer(true, "a1.log", "a2.log"), now)
	source := pathCompletionSource{find: func(dir, base string) listingMatch { return c.findListing(dir, base, now) }}
	line := ":reload --files /big/a"
	got, _ := completeFilesValue(line, len(line), completeGrepArgs, source)
	if got.fetch != "/big/a" || got.line != line || len(got.candidates) != 2 || got.hint != "" {
		t.Fatalf("truncated: %+v", got)
	}
	c.storeListing("srv1", "/big/a", listingAnswer(true, "a1.log"), now)
	got, _ = completeFilesValue(line, len(line), completeGrepArgs, source)
	if got.fetch != "" || got.line != ":reload --files /big/a1.log" || !strings.Contains(got.hint, "cut") {
		t.Fatalf("narrowest truncated: %+v", got)
	}
}

func TestReadableFilesName(t *testing.T) {
	for name, want := range map[string]bool{
		"app.log": true, "my file.log": true, "ünï.log": true,
		"": false, "a,b": false, " lead": false, "trail ": false, "tab\tx": false, "nl\n": false,
		"evil\u202egol.exe": false, "zero\u200bwidth.log": false, "bom\ufeff.log": false,
	} {
		if got := readableFilesName(name); got != want {
			t.Errorf("readableFilesName(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestPathListingHintQuotesServerText checks that the server's listing
// error is shown as text: control characters in it are quoted, not written
// to the terminal, while a plain reason is shown as it is.
func TestPathListingHintQuotesServerText(t *testing.T) {
	match := listingMatch{found: true, listing: pathListing{err: "x\x1b]0;owned\x07\x1b[2J\u202e"}}
	got := pathListingHint(match)
	if strings.IndexFunc(got, hiddenRune) >= 0 || !strings.Contains(got, `\x1b`) {
		t.Fatalf("hint %q writes control or format characters", got)
	}
	match.listing.err = "probe: path listing disabled"
	if got := pathListingHint(match); got != "(path listing: probe: path listing disabled)" {
		t.Fatalf("plain hint %q", got)
	}
}

package clients

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

// noFilesTestClient returns a client whose prober sees files, in
// serverless mode or not, with stdin a pipe or a terminal.
func noFilesTestClient(files []string, serverless, stdinPiped bool) *baseClient {
	c, _ := newProbeTestClient(files)
	c.prober.serverless = serverless
	c.prober.stdinPiped = func() bool { return stdinPiped }
	return c
}

// TestNoFilesHints: a session that reads no files (no --files, no
// positional path) says so with the fix wherever field data is asked for:
// :probe and :fields (with the Tab note only where the line editor completes
// paths), Tab after ":fields " and Tab in a --query value.
func TestNoFilesHints(t *testing.T) {
	c := noFilesTestClient(nil, false, false)
	remote := sessionNoFilesText + sessionNoFilesRemoteTab
	if !c.prober.readsNoFiles() || c.prober.catalogSnapshot().NoFilesHint != remote {
		t.Fatal("session without files: readsNoFiles or NoFilesHint not set")
	}

	var plain bytes.Buffer
	if err := c.runProbeCommand(&plain); err != nil {
		t.Fatal(err)
	}
	if want := "nothing to probe: " + sessionNoFilesText + "\n"; plain.String() != want {
		t.Fatalf(":probe plain: %q, want %q", plain.String(), want)
	}
	var editor bytes.Buffer
	if err := c.runProbeCommand(editorWriter{Writer: &editor}); err != nil {
		t.Fatal(err)
	}
	if want := "nothing to probe: " + remote + "\n"; editor.String() != want {
		t.Fatalf(":probe editor: %q, want %q", editor.String(), want)
	}

	for _, table := range []string{"", "STATS"} {
		plain.Reset()
		if err := c.writeFieldsCommand(&plain, table); err != nil {
			t.Fatal(err)
		}
		if plain.String() != sessionNoFilesText+"\n" {
			t.Fatalf(":fields %q: %q", table, plain.String())
		}
	}

	got := completeInteractive(":fields S", len(":fields S"), completeGrepArgs, c.prober.catalogSnapshot)
	if want := "(" + remote + ")"; got.hint != want {
		t.Fatalf(":fields Tab hint %q, want %q", got.hint, want)
	}
	line := ":reload --query 'select "
	got = completeInteractive(line, len(line), completeMapArgs, c.prober.catalogSnapshot)
	if !strings.Contains(got.hint, remote) || strings.Contains(got.hint, queryCatalogHint) {
		t.Fatalf("--query Tab hint %q, want the no-files hint", got.hint)
	}
	// After from (and on an empty query, which Tab starts with from) only
	// the no-files hint: no server can ever answer with a table.
	for _, line := range []string{":reload --query 'from ", ":reload --query '"} {
		got = completeInteractive(line, len(line), completeMapArgs, c.prober.catalogSnapshot)
		if !strings.Contains(got.hint, remote) || strings.Contains(got.hint, tableNameHint) {
			t.Fatalf("%q Tab hint %q, want only the no-files hint", line, got.hint)
		}
	}
}

// TestNoFilesStdin: "-" (what a serverless session without files reads)
// counts as reading only in serverless mode with stdin not a terminal. On a
// terminal, where interactive dtail is the default, it reads nothing, and
// the hint names local paths; dserver never reads a client's stdin.
func TestNoFilesStdin(t *testing.T) {
	tests := []struct {
		name                   string
		serverless, stdinPiped bool
		want                   string
	}{
		{name: "serverless terminal", serverless: true, want: sessionNoFilesText + sessionNoFilesLocalTab},
		{name: "serverless pipe", serverless: true, stdinPiped: true},
		{name: "server mode pipe", stdinPiped: true, want: sessionNoFilesText + sessionNoFilesRemoteTab},
	}
	for _, tt := range tests {
		c := noFilesTestClient([]string{probeStdin}, tt.serverless, tt.stdinPiped)
		if got := c.prober.noFilesHint(true); got != tt.want {
			t.Fatalf("%s: hint %q, want %q", tt.name, got, tt.want)
		}
		if got := c.prober.catalogSnapshot().NoFilesHint; got != tt.want {
			t.Fatalf("%s: snapshot hint %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestNoFilesHintsNotForFiles is the negative side: a session that reads a
// file (with or without stdin) is no session without files, and an empty
// catalog keeps the texts that the probe has not answered yet.
func TestNoFilesHintsNotForFiles(t *testing.T) {
	for _, files := range [][]string{{"/var/log/app.log"}, {probeStdin, "/var/log/app.log"}} {
		c := noFilesTestClient(files, true, false)
		assertNoFilesHintGone(t, c)
	}
	var nilProber *interactiveProber
	if nilProber.readsNoFiles() || nilProber.noFilesHint(true) != "" {
		t.Fatal("nil prober reports no files")
	}
}

// TestNoFilesHintGoneAfterReload: once a :reload gave the session files
// (storeReloadState, what an applied reload commits), :fields and the Tab
// hints no longer show the no-files hint.
func TestNoFilesHintGoneAfterReload(t *testing.T) {
	c := noFilesTestClient(nil, false, false)
	if c.prober.noFilesHint(false) == "" {
		t.Fatal("no hint before the reload")
	}
	spec := c.sessionSpec
	spec.Files = []string{"/var/log/app.log"}
	c.storeReloadState(config.Args{What: "/var/log/app.log"}, spec)
	assertNoFilesHintGone(t, c)
}

// assertNoFilesHintGone checks that :fields, :probe and the Tab hints of c
// show no no-files hint but the texts of an empty catalog.
func assertNoFilesHintGone(t *testing.T, c *baseClient) {
	t.Helper()
	if c.prober.readsNoFiles() || c.prober.catalogSnapshot().NoFilesHint != "" {
		t.Fatalf("files %q: reported as reading no files", c.sessionSpec.Files)
	}
	var tty bytes.Buffer
	if err := c.writeFieldsCommand(&tty, ""); err != nil {
		t.Fatal(err)
	}
	if tty.String() != fieldsEmptyMessage+"\n" {
		t.Fatalf("files %q: :fields %q", c.sessionSpec.Files, tty.String())
	}
	got := completeInteractive(":fields S", len(":fields S"), completeGrepArgs, c.prober.catalogSnapshot)
	if got.hint != fieldsTableHint {
		t.Fatalf("files %q: :fields Tab hint %q", c.sessionSpec.Files, got.hint)
	}
	line := ":reload --query 'select "
	got = completeInteractive(line, len(line), completeMapArgs, c.prober.catalogSnapshot)
	if !strings.Contains(got.hint, queryCatalogHint) || strings.Contains(got.hint, sessionNoFilesText) {
		t.Fatalf("files %q: --query Tab hint %q", c.sessionSpec.Files, got.hint)
	}
}

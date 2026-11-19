package clients

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

// testInteractiveStyle returns the style of the default theme.
func testInteractiveStyle() *interactiveStyle {
	return newInteractiveStyle(&config.ClientConfig{TermColorsEnable: true,
		TermColors: config.DefaultTermColors()})
}

func TestNewInteractiveStyleNeedsColors(t *testing.T) {
	if newInteractiveStyle(nil) != nil {
		t.Fatal("style without a client config, want nil")
	}
	off := &config.ClientConfig{TermColorsEnable: false, TermColors: config.DefaultTermColors()}
	if newInteractiveStyle(off) != nil {
		t.Fatal("style with colors disabled (--noColor, --plain), want nil")
	}
	if testInteractiveStyle() == nil {
		t.Fatal("no style with colors enabled")
	}
	if styleOf(&bytes.Buffer{}) != nil {
		t.Fatal("a plain writer has a style, want nil (the plain line reading paints nothing)")
	}
	styled := editorWriter{Writer: &bytes.Buffer{}, styled: testInteractiveStyle()}
	if styleOf(styled) == nil {
		t.Fatal("the editor's writer lost its style")
	}
}

// TestInteractiveStyleOffIsPlain checks that a nil style returns every text
// unchanged: the output of --noColor, --plain and the plain line reading.
func TestInteractiveStyleOffIsPlain(t *testing.T) {
	var s *interactiveStyle
	help := interactiveHelpText(config.Args{Mode: omode.GrepClient})
	fields := fieldsText(queryCatalog, "", time.Unix(100, 0))
	for name, pair := range map[string][2]string{
		"banner":  {s.banner(interactiveBanner), interactiveBanner},
		"failure": {s.failure("reload failed: x"), "reload failed: x"},
		"help":    {s.help(help), help},
		"fields":  {s.fields(fields), fields},
		"prompt":  {s.prompt(), interactivePrompt},
		"show": {s.keyValues([]keyValue{{key: "mode", value: "grep"}, {key: "regex", value: `"a b"`}}),
			`mode=grep regex="a b"`},
		"candidates": {s.listing(completion{candidates: []string{"b ", "$a ", "a/"}, kind: candidatePath}, 80),
			"b  $a  a/"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s without colors = %q, want %q", name, pair[0], pair[1])
		}
	}
}

// TestInteractiveStylePaints checks that the default theme paints each kind
// of control output and changes nothing but the color sequences.
func TestInteractiveStylePaints(t *testing.T) {
	s := testInteractiveStyle()
	theme := config.DefaultTermColors().Interactive
	help := interactiveHelpText(config.Args{Mode: omode.GrepClient})
	fields := fieldsText(queryCatalog, "", time.Unix(100, 0))
	painted := map[string][2]string{
		"failure": {s.failure("reload failed"), "reload failed"},
		"help":    {s.help(help), help},
		"fields":  {s.fields(fields), fields},
		"prompt":  {s.prompt(), interactivePrompt},
	}
	// A catalog with probe errors, notes and a truncated answer.
	full := fieldsText(fieldsCatalog, "", fieldsCatalog.Updated)
	painted["fields with errors"] = [2]string{s.fields(full), full}
	for name, pair := range painted {
		if pair[0] == pair[1] {
			t.Errorf("%s is not painted: %q", name, pair[0])
		}
		if got := stripANSI(pair[0]); got != pair[1] {
			t.Errorf("%s without its colors = %q, want %q", name, got, pair[1])
		}
	}
	if want := string(theme.Error.Fg) + string(theme.Error.Attr) + "reload failed" +
		string(color.AttrReset) + string(color.FgDefault); painted["failure"][0] != want {
		t.Errorf("failure = %q, want %q", painted["failure"][0], want)
	}
	command := s.paint(theme.Command, ":reload --grep ERROR")
	if !strings.Contains(painted["help"][0], command+" ") {
		t.Errorf("help example command not painted as a command: %q", painted["help"][0])
	}
	if !strings.Contains(painted["fields"][0], s.paint(theme.Key, "goroutines")) {
		t.Errorf("field name not painted as a key: %q", painted["fields"][0])
	}
}

// TestInteractiveStyleCandidates checks that listed candidates are painted
// in their kind's style: query groups per group (a $variable of a field
// group as a variable), paths as paths and directories.
func TestInteractiveStyleCandidates(t *testing.T) {
	s := testInteractiveStyle()
	theme := config.DefaultTermColors().Interactive
	result := completion{
		candidates: []string{"from ", "status ", "$hostname ", "count("},
		groups: []candidateGroup{
			{label: "next", kind: candidateKeyword, candidates: []string{"from "}},
			{label: "field", kind: candidateField, candidates: []string{"status ", "$hostname "}},
			{label: "aggregation", kind: candidateFunction, candidates: []string{"count("}},
		},
	}
	want := strings.Join([]string{
		s.paint(theme.Heading, "next:") + "        " + s.paint(theme.CandidateKeyword, "from"),
		s.paint(theme.Heading, "field:") + "       " + s.paint(theme.CandidateField, "status") + "  " +
			s.paint(theme.CandidateVariable, "$hostname"),
		s.paint(theme.Heading, "aggregation:") + " " + s.paint(theme.CandidateFunction, "count"),
	}, "\n")
	if got := s.listing(result, 80); got != want {
		t.Fatalf("listing = %q, want %q", got, want)
	}
	paths := completion{candidates: []string{"app.log", "sub/"}, kind: candidatePath}
	want = s.paint(theme.CandidatePath, "app.log") + "  " + s.paint(theme.CandidateDirectory, "sub/")
	if got := s.listing(paths, 80); got != want {
		t.Fatalf("path listing = %q, want %q", got, want)
	}
}

// TestQueryCompletionKinds checks the kinds of the groups a query
// completion records for its candidates.
func TestQueryCompletionKinds(t *testing.T) {
	kindOf := func(result queryCompletion, candidate string) (candidateKind, bool) {
		for _, group := range result.groups {
			if slices.Contains(group.candidates, candidate) {
				return group.kind, true
			}
		}
		return 0, false
	}
	result := completeQueryWord("from STATS select ", "", queryCatalog)
	if got, ok := kindOf(result, "count("); !ok || got != candidateFunction {
		t.Errorf("kind of count( = %d (%v), want a function", got, ok)
	}
	keywords := completeQueryWord("fr", "", emptyCatalog())
	from := keywords.candidates[0]
	if got, _ := kindOf(keywords, from); from != "from " || got != candidateKeyword {
		t.Errorf("kind of %q = %d, want the keyword from", from, got)
	}
}

func TestStripANSI(t *testing.T) {
	for in, want := range map[string]string{
		"":                                      "",
		"plain":                                 "plain",
		"\x1b[33m\x1b[1mdtail> \x1b[0m\x1b[39m": "dtail> ",
		"a\x1b[":                                "a\x1b[",
	} {
		if got := stripANSI(in); got != want {
			t.Errorf("stripANSI(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEditorScreenCountsPrompt checks that the rows of the edit line count
// the prompt in front of it.
func TestEditorScreenCountsPrompt(t *testing.T) {
	screen := newEditorScreen(nil)
	screen.setWidth(10)
	line := strings.Repeat("x", 5)
	if row := screen.cursorRow(line, len(line)); row != 0 {
		t.Fatalf("row without a prompt = %d, want 0", row)
	}
	screen.setPromptWidth(len(interactivePrompt))
	if row := screen.cursorRow(line, len(line)); row != 1 {
		t.Fatalf("row with a %d column prompt = %d, want 1", len(interactivePrompt), row)
	}
}

// TestInteractiveStylePaintsEachLine checks that a text of several lines
// (an error joined with errors.Join) is painted line by line, as :help and
// :fields are: every line starts in its style and ends reset, so no color
// runs across a line end and an empty line stays empty.
func TestInteractiveStylePaintsEachLine(t *testing.T) {
	s := testInteractiveStyle()
	theme := config.DefaultTermColors().Interactive
	text := "reload failed: first\n\nsecond"
	want := s.paint(theme.Error, "reload failed: first") + "\n\n" + s.paint(theme.Error, "second")
	if got := s.failure(text); got != want {
		t.Fatalf("failure of several lines = %q, want %q", got, want)
	}
	if got, want := s.failure("one line"), string(theme.Error.Fg)+string(theme.Error.Attr)+"one line"+
		string(color.AttrReset)+string(color.FgDefault); got != want {
		t.Fatalf("failure of one line = %q, want %q", got, want)
	}
	var off *interactiveStyle
	if got := off.failure(text); got != text {
		t.Fatalf("failure without colors = %q, want %q", got, text)
	}
}

// TestInteractiveStyleFieldsEmptyIsHint checks that the answer of :fields
// while no server answered a probe yet is painted as a hint, not as the
// heading of a catalog.
func TestInteractiveStyleFieldsEmptyIsHint(t *testing.T) {
	s := testInteractiveStyle()
	theme := config.DefaultTermColors().Interactive
	empty := fieldsText(catalogSnapshot{}, "", time.Unix(100, 0))
	if got, want := s.fields(empty), s.paint(theme.Hint, fieldsEmptyMessage); got != want {
		t.Fatalf("empty :fields = %q, want the hint %q", got, want)
	}
}

// TestEditorPromptOnNarrowTerminal checks the prompt of a line started while
// the output is paused: painted where the terminal is at least as wide as
// the painted prompt has runes, plain on a narrower one (x/term cuts the
// prompt into rows by runes, escape sequences included, and miscounts the
// cursor where a cut falls into one), none while the output runs.
func TestEditorPromptOnNarrowTerminal(t *testing.T) {
	s := testInteractiveStyle()
	painted := utf8.RuneCountInString(s.prompt())
	tests := []struct {
		name   string
		style  *interactiveStyle
		paused bool
		width  int
		want   string
	}{
		{name: "running", style: s, paused: false, width: 80, want: ""},
		{name: "wide", style: s, paused: true, width: 80, want: s.prompt()},
		{name: "as wide as the painted prompt", style: s, paused: true, width: painted, want: s.prompt()},
		{name: "one column less", style: s, paused: true, width: painted - 1, want: interactivePrompt},
		{name: "24 columns", style: s, paused: true, width: 24, want: interactivePrompt},
		{name: "17 columns", style: s, paused: true, width: 17, want: interactivePrompt},
		{name: "no colors, narrow", style: nil, paused: true, width: 10, want: interactivePrompt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.style.editorPrompt(tt.paused, tt.width); got != tt.want {
				t.Fatalf("editorPrompt(paused %v, width %d) = %q, want %q", tt.paused, tt.width, got, tt.want)
			}
		})
	}
}

package clients

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

// listingStep is one Tab press whose candidates are listed: the line typed
// and the listing the editor prints for it (without its final newline).
type listingStep struct {
	line, listing string
}

// targetListingSteps are the listings of the target session of the guided
// query builder (statsOnlyCatalog, plus the where steps of guideCatalog's
// STATS table): a heading saying what is expected where the groups' labels
// do not, labelled groups of bare names, fields before $variables, and the
// hint on its own line.
var targetListingSteps = []listingStep{
	{":query from STATS select ", strings.Join([]string{
		"select item",
		"aggregation: count  sum  min  max  avg  last  len  percentage  percentile",
		"field:       currentConnections  lifetimeConnections",
		"variable:    $hostname  $time"}, "\n")},
	{":query from STATS select count(", strings.Join([]string{
		"field to count",
		"field:    currentConnections  lifetimeConnections",
		"variable: $hostname  $time"}, "\n")},
	{":query from STATS select count(currentConnections) ", strings.Join([]string{
		"next: where  group by  order by  limit",
		commaItemHint}, "\n")},
	// Right after the ) of a finished item: the same as after the blank.
	{":query from STATS select count(currentConnections)", strings.Join([]string{
		"next: where  group by  order by  limit",
		commaItemHint}, "\n")},
	{":query from STATS select count(currentConnections) where currentConnections == ", strings.Join([]string{
		"value for currentConnections",
		"value: 0  3"}, "\n")},
	{":query from STATS select count(currentConnections) where ", strings.Join([]string{
		"field to filter on",
		"field:    currentConnections  lifetimeConnections",
		"variable: $hostname  $time"}, "\n")},
	// and and the next clauses are one "next" group, without a heading.
	{":query from STATS select count(currentConnections) where currentConnections > 0 ", strings.Join([]string{
		"next: and  group by  order by  limit",
		commaConditionHint}, "\n")},
	// Before the first clause: the clauses, no heading.
	{":query l", "clause: limit  logformat"},
	// A typed word after a finished item: next clauses beside a field,
	// which no one heading describes.
	{":query from STATS select count(currentConnections) li", strings.Join([]string{
		"next:  limit",
		"field: lifetimeConnections"}, "\n")},
	{":query from STATS select count(currentConnections), sum(lifetimeConnections) order by ",
		strings.Join([]string{
			"to order by",
			"select item: count(currentConnections)  sum(lifetimeConnections)"}, "\n")},
}

// TestQueryListingTargetSession checks the listing text of every step of
// the target session without colors, and that the painted listing is the
// same text with the groups painted per kind and the heading and labels in
// the heading style.
func TestQueryListingTargetSession(t *testing.T) {
	theme := config.DefaultTermColors().Interactive
	painted := testInteractiveStyle()
	for _, step := range targetListingSteps {
		result := completeInteractive(step.line, len(step.line), guideArgs, catalogOf(statsOnlyCatalog))
		var plain *interactiveStyle
		if got := plain.listing(result, 80); got != step.listing {
			t.Errorf("%q: listing\n%s\nwant\n%s", step.line, got, step.listing)
		}
		colored := painted.listing(result, 80)
		if got := stripANSI(colored); got != step.listing {
			t.Errorf("%q: painted listing without colors\n%s\nwant\n%s", step.line, got, step.listing)
		}
		if colored == step.listing {
			t.Errorf("%q: listing not painted", step.line)
		}
	}
	result := completeInteractive(targetListingSteps[0].line, len(targetListingSteps[0].line), guideArgs,
		catalogOf(statsOnlyCatalog))
	colored := painted.listing(result, 80)
	for _, want := range []string{
		painted.paint(theme.Heading, "select item"),
		painted.paint(theme.Heading, "aggregation:"),
		painted.paint(theme.CandidateFunction, "count"),
		painted.paint(theme.CandidateField, "currentConnections"),
		painted.paint(theme.CandidateVariable, "$hostname"),
	} {
		if !strings.Contains(colored, want) {
			t.Errorf("painted listing %q lacks %q", colored, want)
		}
	}
}

// TestQueryListingNarrow checks that a long group wraps at the terminal
// width, its rows aligned under the first, and is cut after
// listingMaxRows rows with the number of names left out (the short marker
// where the long one does not fit).
func TestQueryListingNarrow(t *testing.T) {
	line := ":query from STATS select "
	result := completeInteractive(line, len(line), guideArgs, catalogOf(statsOnlyCatalog))
	var plain *interactiveStyle
	want := strings.Join([]string{
		"select item",
		"aggregation: count  sum",
		"             min  max  avg",
		"             ... (4 more)",
		"field:       currentConnections",
		"             lifetimeConnections",
		"variable:    $hostname",
		"             $time"}, "\n")
	if got := plain.listing(result, 26); got != want {
		t.Fatalf("narrow listing\n%s\nwant\n%s", got, want)
	}
	for _, row := range strings.Split(plain.listing(result, 40), "\n") {
		if len(row) > 40 {
			t.Errorf("row %q is wider than 40 columns", row)
		}
	}
	var paths []string
	for i := range 100 {
		paths = append(paths, fmt.Sprintf("file%02d.log", i))
	}
	want = strings.Join([]string{
		"file00.log  file01.log  file02.log  file03.log  file04.log  file05.log",
		"file06.log  file07.log  file08.log  file09.log  file10.log  file11.log",
		"file12.log  file13.log  file14.log  file15.log  ... (84 more, type to narrow)"}, "\n")
	if got := plain.listing(completion{candidates: paths, kind: candidatePath}, 80); got != want {
		t.Fatalf("cut path listing\n%s\nwant\n%s", got, want)
	}
}

// TestBareCandidateName checks the names a listing shows for what Tab
// inserts: without the blank, parentheses and quotes added on insert.
func TestBareCandidateName(t *testing.T) {
	for in, want := range map[string]string{
		"count(":    "count",
		"field)":    "field",
		"count(x) ": "count(x)",
		`"web1" `:   "web1",
		`web1" `:    "web1",
		// A keyword field keeps the backticks it must be written in; the
		// closing backtick of a word inside an open one is dropped.
		"`from` ": "`from`",
		"from` ":  "from",
		`"a(" `:   "a(",
		// Backticks inside double quotes only protect the quoted word:
		// the value by, the value `w` and a quoted field a(b).
		"\"`by`\" ":    "by",
		"`by`\" ":      "by",
		"\"``w``\" ":   "`w`",
		"\"`a(b)`\" ":  "a(b)",
		"group by ":    "group by",
		"$hostname ":   "$hostname",
		"currentConn)": "currentConn",
	} {
		if got := bareCandidateName(in); got != want {
			t.Errorf("bareCandidateName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestListingWideRunes checks that East Asian wide names count two
// columns each, so a group of them wraps before the terminal would.
func TestListingWideRunes(t *testing.T) {
	for in, want := range map[string]int{"": 0, "abc": 3, "日本": 4, "Ｆ": 2, "é": 1, "a漢b": 4} {
		if got := displayWidth(in); got != want {
			t.Errorf("displayWidth(%q) = %d, want %d", in, got, want)
		}
	}
	var plain *interactiveStyle
	result := completion{candidates: []string{"日本語 ", "漢字 "}, heading: "select item",
		groups: []candidateGroup{{label: "field", kind: candidateField, candidates: []string{"日本語 ", "漢字 "}}}}
	// field: is 7 columns, 日本語 6 and 漢字 4: 7+6+2+4 = 19 > 18.
	if got, want := plain.listing(result, 18), "select item\nfield: 日本語\n       漢字"; got != want {
		t.Errorf("wide listing = %q, want %q", got, want)
	}
}

// TestListingUngrouped checks that commands, flags and paths, which have
// no groups, are listed on one wrapped line as before.
func TestListingUngrouped(t *testing.T) {
	var plain *interactiveStyle
	result := completion{candidates: []string{":reload ", ":show", ":fields "}}
	if got := plain.listing(result, 80); got != ":reload  :show  :fields" {
		t.Fatalf("commands listing %q", got)
	}
	paths := completion{candidates: []string{"a.log", "sub/"}, kind: candidatePath,
		header: "listing of srv1", hint: "a hint"}
	if got := plain.listing(paths, 80); got != "listing of srv1\na.log  sub/\na hint" {
		t.Fatalf("path listing %q", got)
	}
}

// TestListingWithoutCandidates checks that a completion without candidates
// lists only its header and hint, never a heading or an empty group, and
// that a terminal narrower than the labels still lists every row.
func TestListingWithoutCandidates(t *testing.T) {
	var plain *interactiveStyle
	for _, result := range []completion{
		{},
		{heading: "field to count", groups: []candidateGroup{{label: "field"}}},
	} {
		if got := plain.listing(result, 80); got != "" {
			t.Errorf("listing of %+v = %q, want nothing", result, got)
		}
	}
	if got := plain.listing(completion{heading: "next", hint: commaItemHint}, 80); got != commaItemHint {
		t.Errorf("hint-only listing = %q", got)
	}
	result := completion{candidates: []string{"sum(", "count("}, heading: "select item",
		groups: []candidateGroup{{label: "aggregation", kind: candidateFunction,
			candidates: []string{"sum(", "count("}}}}
	want := "select item\naggregation: sum\n             count"
	if got := plain.listing(result, 5); got != want {
		t.Errorf("listing at width 5 = %q, want %q", got, want)
	}
}

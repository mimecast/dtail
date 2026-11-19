package clients

import (
	"fmt"
	"slices"
	"strings"
)

// This file renders what Tab lists when it can not extend the word: a
// header (which server a path listing came from), a heading saying what is
// expected at the cursor ("field to count", "value for x"), the
// candidates in labelled groups, one group per line ("aggregation:",
// "field:", "variable:", "next:"), and a hint line. Query candidates are
// listed as bare names: the "(", ")", closing quote and blank that Tab
// inserts with them are left out (bareCandidateName). A group longer than
// the terminal is wide wraps, its rows aligned under its first, and is cut
// after listingMaxRows rows with the number of names left out.

const (
	// listingMaxRows is the number of rows a group takes at most; the last
	// one ends with listingMoreMarker when names are left out.
	listingMaxRows = 3
	// listingMoreMarker and listingMoreShort end the last row of a cut
	// group (the short one where the long one does not fit).
	listingMoreMarker = "... (%d more, type to narrow)"
	listingMoreShort  = "... (%d more)"
	// listingSeparator separates the names of a row.
	listingSeparator = "  "
)

// listedGroup is one group of a listing: its label (empty for the one
// group of commands, flags and paths) and its names as shown, with their
// kinds, which decide their style.
type listedGroup struct {
	label string
	names []string
	kinds []candidateKind
}

// listing returns the text the editor prints for a completion on a
// terminal width columns wide, without a final newline, "" when there is
// nothing to show. With a style the header and hint are painted as hints,
// the heading and the labels as headings and every name in the style of
// its kind; without one it is the same text unpainted.
func (s *interactiveStyle) listing(result completion, width int) string {
	groups := listedGroups(result)
	if len(groups) == 0 {
		return joinLines(s.hint(result.header), s.hint(result.hint))
	}
	heading := result.heading
	if labelsSay(groups, heading) {
		heading = "" // "table" above "table: STATS" says nothing more.
	}
	labelWidth := 0
	for _, group := range groups {
		if group.label != "" {
			labelWidth = max(labelWidth, displayWidth(group.label)+len(": "))
		}
	}
	rows := []string{s.hint(result.header), s.paint(s.themeOf().Heading, heading)}
	for _, group := range groups {
		rows = append(rows, s.groupRows(group, labelWidth, width)...)
	}
	return joinLines(append(rows, s.hint(result.hint))...)
}

// listedGroups returns the groups of a completion to list: its query
// groups with bare names, else its candidates as one unlabelled group
// (commands, flags, paths) without their trailing blanks.
func listedGroups(result completion) []listedGroup {
	if len(result.candidates) == 0 {
		return nil
	}
	if len(result.groups) == 0 {
		group := listedGroup{names: trimEach(result.candidates)}
		for _, name := range group.names {
			group.kinds = append(group.kinds, nameKind(result.kind, name))
		}
		return []listedGroup{group}
	}
	// Groups of one label (and, the next clauses, by and append are all
	// "next") are listed as one, where the first of them is.
	var groups []listedGroup
	at := map[string]int{}
	for _, g := range result.groups {
		i, ok := at[g.label]
		if !ok {
			i = len(groups)
			at[g.label] = i
			groups = append(groups, listedGroup{label: g.label})
		}
		group := &groups[i]
		for _, c := range g.candidates {
			if name := bareCandidateName(c); !slices.Contains(group.names, name) {
				group.names = append(group.names, name)
				group.kinds = append(group.kinds, nameKind(g.kind, name))
			}
		}
	}
	return groups
}

// labelsSay reports whether every group is labelled heading, which then
// says nothing the labels do not.
func labelsSay(groups []listedGroup, heading string) bool {
	for _, group := range groups {
		if group.label != heading {
			return false
		}
	}
	return true
}

// nameKind returns the kind a name of a group of kind is painted as: a
// path ending in a slash is a directory, a field starting with $ a
// variable.
func nameKind(kind candidateKind, name string) candidateKind {
	switch {
	case kind == candidatePath && strings.HasSuffix(name, "/"):
		return candidateDirectory
	case kind == candidateField && strings.HasPrefix(name, "$"):
		return candidateVariable
	}
	return kind
}

// groupRows returns the rows of one group: its label padded to labelWidth
// columns in front of the first row, the same indent in front of the
// others, the names packed into width columns (packRows).
func (s *interactiveStyle) groupRows(group listedGroup, labelWidth, width int) []string {
	rows, more := packRows(group.names, max(width-labelWidth, 1))
	lines := make([]string, len(rows))
	next := 0
	for i, row := range rows {
		painted := make([]string, len(row))
		for j, name := range row {
			painted[j] = s.paint(s.kindStyle(group.kinds[next+j]), name)
		}
		next += len(row)
		if i == len(rows)-1 && more > 0 {
			painted = append(painted, moreMarker(row, more, width-labelWidth))
		}
		lines[i] = strings.Repeat(" ", labelWidth) + strings.Join(painted, listingSeparator)
	}
	if group.label != "" {
		label := group.label + ":"
		pad := strings.Repeat(" ", labelWidth-displayWidth(label))
		lines[0] = s.paint(s.themeOf().Heading, label) + pad + lines[0][labelWidth:]
	}
	return lines
}

// packRows packs names into rows of at most width columns (a name wider
// than that gets a row of its own), at most listingMaxRows of them. When
// they do not all fit, the last row keeps only as many names as fit in
// front of the marker of the names left out, whose number it returns.
func packRows(names []string, width int) ([][]string, int) {
	rows := fillRows(names, width)
	if len(rows) <= listingMaxRows {
		return rows, 0
	}
	rows = rows[:listingMaxRows-1]
	placed := 0
	for _, row := range rows {
		placed += len(row)
	}
	var last []string
	for _, name := range names[placed:] {
		left := len(names) - placed - len(last) - 1
		if rowWidth(append(slices.Clone(last), name, moreText(left, width))) > width {
			break
		}
		last = append(last, name)
	}
	return append(rows, last), len(names) - placed - len(last)
}

// fillRows packs names greedily into rows of at most width columns.
func fillRows(names []string, width int) [][]string {
	var rows [][]string
	var row []string
	for _, name := range names {
		if len(row) > 0 && rowWidth(append(slices.Clone(row), name)) > width {
			rows, row = append(rows, row), nil
		}
		row = append(row, name)
	}
	return append(rows, row)
}

// rowWidth returns the columns of names joined by listingSeparator.
func rowWidth(names []string) int {
	return displayWidth(strings.Join(names, listingSeparator))
}

// moreText returns the marker for n names left out: the long one when it
// fits into width columns, else the short one.
func moreText(n, width int) string {
	if text := fmt.Sprintf(listingMoreMarker, n); displayWidth(text) <= width {
		return text
	}
	return fmt.Sprintf(listingMoreShort, n)
}

// moreMarker returns the marker ending a cut group's last row (row): the
// long one when it fits behind the row into width columns.
func moreMarker(row []string, n, width int) string {
	if len(row) > 0 {
		width -= rowWidth(row) + len(listingSeparator)
	}
	return moreText(n, width)
}

// bareCandidateName returns the name a query candidate is listed as,
// without what Tab inserts around it: the trailing blank; the double
// quotes of a value or quoted name, written or only closing (inside an
// open quote), and the backticks inside them that only protect the quoted
// word from the parser ("`by`" lists as by); the closing backtick of a
// word typed inside an open one; an opening parenthesis after an
// aggregation or function (count) or a closing one after a field inside a
// call (field), unless the name holds the opening one (count(x) of order
// by). A name in backticks (a field named like a keyword, `from`) keeps
// them, as it must be written so.
func bareCandidateName(candidate string) string {
	name := strings.TrimRight(candidate, " ")
	if inner, ok := strings.CutSuffix(name, `"`); ok {
		inner = strings.TrimPrefix(inner, `"`)
		if isBacktickWrapped(inner) {
			inner = inner[1 : len(inner)-1]
		}
		return inner
	}
	if isBacktickWrapped(name) {
		return name
	}
	if trimmed, ok := strings.CutSuffix(name, "`"); ok {
		return trimmed
	}
	if trimmed, ok := strings.CutSuffix(name, "("); ok {
		return trimmed
	}
	if trimmed, ok := strings.CutSuffix(name, ")"); ok && !strings.Contains(trimmed, "(") {
		return trimmed
	}
	return name
}

// displayWidth returns the terminal columns of s: two for a rune of the
// East Asian Wide and Fullwidth ranges in wideRanges, one for any other.
// Combining and zero-width runes count one column too, so a name made of
// them may overrun the width by a few columns; the terminal then wraps
// the row (a full width table, e.g. golang.org/x/text/width, would be a
// new dependency).
func displayWidth(s string) int {
	n := 0
	for _, r := range s {
		n++
		for _, wide := range wideRanges {
			if r >= wide[0] && r <= wide[1] {
				n++
				break
			}
		}
	}
	return n
}

// wideRanges are the main East Asian Wide and Fullwidth ranges: Hangul
// Jamo, CJK radicals to CJK compatibility, CJK ideographs and extensions,
// Yi, Hangul syllables, compatibility ideographs, vertical and small
// forms, fullwidth forms, the emoji blocks and the supplementary
// ideographic planes.
var wideRanges = [][2]rune{
	{0x1100, 0x115F}, {0x2E80, 0x303E}, {0x3041, 0x33FF}, {0x3400, 0x4DBF},
	{0x4E00, 0x9FFF}, {0xA000, 0xA4CF}, {0xAC00, 0xD7A3}, {0xF900, 0xFAFF},
	{0xFE30, 0xFE4F}, {0xFF00, 0xFF60}, {0xFFE0, 0xFFE6}, {0x1F300, 0x1F64F},
	{0x1F900, 0x1F9FF}, {0x20000, 0x3FFFD},
}

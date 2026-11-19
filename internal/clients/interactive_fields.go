package clients

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// fieldsEmptyMessage answers :fields while no server answered a probe yet.
const fieldsEmptyMessage = "no field data from the servers yet: the background probe runs when the session " +
	"starts, after every :reload and every minute while the output runs; :probe probes now"

// fieldsTableHint is listed when Tab completes a :fields table while no
// server answered a probe yet.
const fieldsTableHint = "(no field data from the servers yet: :probe probes now)"

// fieldsSampleIndent indents a field's or $variable's line under its table.
const fieldsSampleIndent = "    "

// fieldsSamplesSeparator separates a field's name from its sample values.
const fieldsSamplesSeparator = "  e.g. "

// fieldsMaxNameWidth caps the column the sample values of a :fields table
// are aligned to, so one long field name does not push all values right.
const fieldsMaxNameWidth = 24

// runProbeCommand answers :probe: it requests a probe round of the
// background prober now and reports what came of it, or that its answers
// are stored once they arrive. The caller resumed the output before (see
// handleInteractiveLine), as a server's answer can only be read while the
// output runs. A session without files has nothing to probe and is told
// how to give it files instead.
func (c *baseClient) runProbeCommand(tty io.Writer) error {
	if hint := c.prober.noFilesHint(completesPaths(tty)); hint != "" {
		return writeControlLine(tty, styleOf(tty).hint("nothing to probe: "+hint))
	}
	result, status := c.prober.probeNow()
	return writeControlLine(tty, styleOf(tty).hint(probeResultMessage(result, status)))
}

// writeFieldsCommand answers :fields [table] with the field catalog as of
// now (fieldsLines), or, for a session without files, whose catalog stays
// empty, with how to give it files.
func (c *baseClient) writeFieldsCommand(tty io.Writer, table string) error {
	if hint := c.prober.noFilesHint(completesPaths(tty)); hint != "" {
		return writeControlLine(tty, styleOf(tty).hint(hint))
	}
	now := time.Now
	if c.prober != nil {
		now = c.prober.now
	}
	return writeControlLine(tty, styleOf(tty).fields(fieldsLines(c.prober.catalogSnapshot(), table, now())))
}

// parseFieldsCommand parses ":fields" with an optional table name.
func parseFieldsCommand(trimmed string) (interactiveCommand, error) {
	words := strings.Fields(strings.TrimPrefix(trimmed, ":fields"))
	if len(words) > 1 {
		return interactiveCommand{}, fmt.Errorf(":fields takes at most one table name, got %d words", len(words))
	}
	command := interactiveCommand{kind: "fields"}
	if len(words) == 1 {
		command.table = words[0]
	}
	return command, nil
}

// probeResultMessage is the answer of :probe for the result of its round.
func probeResultMessage(result probeRoundResult, status probeWaitStatus) string {
	switch {
	case status == probeWaitStopped:
		return "probe not run: the background prober is not running"
	case status == probeWaitTimeout:
		return fmt.Sprintf("probe requested, no answer within %s: answers that arrive later are stored "+
			"for :fields and Tab completion", probeNowWait)
	case result.files == 0:
		return "nothing to probe: the session reads no files (stdin is never probed)"
	case len(result.answered)+len(result.busy)+len(result.failed) == 0:
		return "nothing to probe: no server connection is up"
	}
	var parts []string
	if len(result.answered) > 0 {
		parts = append(parts, "answered by "+displayList(result.answered))
	}
	if len(result.busy) > 0 {
		parts = append(parts, "busy with another probe: "+displayList(result.busy))
	}
	if len(result.failed) > 0 {
		parts = append(parts, "no answer from "+displayList(result.failed)+" (see the debug log)")
	}
	message := "probe " + strings.Join(parts, "; ")
	if len(result.answered) > 0 {
		message += "; :fields shows what was found"
	}
	return message
}

// fieldsLineKind is what a line of the :fields answer is, which decides
// its style (interactiveStyle.fields).
type fieldsLineKind int

const (
	// fieldsPlain is a line without a style of its own: a note, a table
	// probed on its own, the cut-off remark.
	fieldsPlain fieldsLineKind = iota
	// fieldsHint is the answer without field data (fieldsEmptyMessage).
	fieldsHint
	// fieldsHeading is the header and a log format, table or $variables
	// line.
	fieldsHeading
	// fieldsError is a probe error or a table that was not found.
	fieldsError
	// fieldsSample is a field or $variable with its sample values.
	fieldsSample
)

// fieldsLine is one line of the :fields answer: what it is and its text.
// The answer is built as such lines and painted from them, never by
// reading its text again: names and notes come from the servers, so a
// field named "a  e.g. b" or a note ending in ":" would be taken for
// another kind of line.
type fieldsLine struct {
	kind fieldsLineKind
	// text is the line; of a fieldsSample its indented name only.
	text string
	// pad and values are the blanks aligning the sample values of a
	// fieldsSample line and those values; both empty for a name without
	// values.
	pad, values string
}

// String returns the line as plain text.
func (l fieldsLine) String() string {
	if l.values == "" {
		return l.text
	}
	return l.text + l.pad + fieldsSamplesSeparator + l.values
}

// fieldsDoc collects the lines of the :fields answer.
type fieldsDoc struct{ lines []fieldsLine }

// add appends a line of kind with the formatted text.
func (d *fieldsDoc) add(kind fieldsLineKind, format string, args ...any) {
	d.lines = append(d.lines, fieldsLine{kind: kind, text: fmt.Sprintf(format, args...)})
}

// fieldsText renders the field catalog for :fields as plain text
// (fieldsLines).
func fieldsText(snap catalogSnapshot, table string, now time.Time) string {
	lines := fieldsLines(snap, table, now)
	texts := make([]string, len(lines))
	for i, line := range lines {
		texts[i] = line.String()
	}
	return strings.Join(texts, "\n")
}

// fieldsLines renders the field catalog for :fields: a header with the
// servers, the age of the data and the default log format, then per log
// format (the default first) its tables with their fields and sample values,
// its $variables and the notes of its probe, then the servers' probe errors.
// A table restricts the listing to the tables of that name (any case, as a
// query's from clause), with the $variables of their log formats. Every
// name and value from a server is shown escaped where it is not plain
// printable text, so that it cannot write control sequences to the terminal.
func fieldsLines(snap catalogSnapshot, table string, now time.Time) []fieldsLine {
	doc := &fieldsDoc{}
	if snap.empty() {
		doc.add(fieldsHint, "%s", fieldsEmptyMessage)
		return doc.lines
	}
	doc.add(fieldsHeading, "%s", fieldsHeader(snap, now))
	formats := fieldsFormatOrder(snap)
	found := false
	for _, name := range formats {
		found = writeFieldsFormat(doc, name, snap.Formats[name], table) || found
	}
	switch {
	case table != "" && !found:
		doc.add(fieldsError, "no table %s found; tables: %s", displayText(table), displayList(catalogTables(snap)))
	case len(formats) == 0:
		doc.add(fieldsPlain, "no log format found fields in the sampled lines")
	}
	writeTableProbes(doc, snap, table)
	for _, probeErr := range snap.Errors {
		file := ""
		if probeErr.File != "" {
			file = fmt.Sprintf(" (%s)", displayText(probeErr.File))
		}
		doc.add(fieldsError, "probe error on %s: %s%s", displayText(probeErr.Server),
			displayText(probeErr.Error), file)
	}
	if snap.Truncated {
		doc.add(fieldsPlain, "(an answer was cut to the size limit: fields or sample values may be missing)")
	}
	return doc.lines
}

// fieldsHeader is the first line of the :fields answer.
func fieldsHeader(snap catalogSnapshot, now time.Time) string {
	age := max(now.Sub(snap.Updated), 0).Round(time.Second)
	header := fmt.Sprintf("fields found by the probe of %s, updated %s ago", displayList(snap.Servers), age)
	if snap.DefaultLogFormat != "" {
		header += "; default log format " + displayText(snap.DefaultLogFormat)
	}
	return header
}

// fieldsFormatOrder returns the log formats of snap, the default one first,
// the others sorted.
func fieldsFormatOrder(snap catalogSnapshot) []string {
	formats := slices.Sorted(maps.Keys(snap.Formats))
	if i := slices.Index(formats, snap.DefaultLogFormat); i > 0 {
		formats = slices.Insert(slices.Delete(formats, i, i+1), 0, snap.DefaultLogFormat)
	}
	return formats
}

// writeTableProbes writes a line per table probed on its own
// (catalogSnapshot.TableProbes) that matches table (any case; every one
// without a table), with the lines its servers sampled.
func writeTableProbes(doc *fieldsDoc, snap catalogSnapshot, table string) {
	for _, probed := range slices.Sorted(maps.Keys(snap.TableProbes)) {
		if table != "" && !strings.EqualFold(probed, table) {
			continue
		}
		doc.add(fieldsPlain, "table %s probed on its own: %d lines of it sampled", displayText(probed),
			snap.TableProbes[probed])
	}
}

// writeFieldsFormat writes the tables, $variables and notes of one log
// format, or only its tables matching table if one is given, each with the
// $variables its lines filled (catalogFormat.variablesOf), and reports
// whether it wrote anything.
func writeFieldsFormat(doc *fieldsDoc, name string, format catalogFormat, table string) bool {
	if table != "" {
		return writeFieldsTables(doc, name, format, table)
	}
	doc.add(fieldsHeading, "log format %s:", displayText(name))
	for _, t := range slices.Sorted(maps.Keys(format.Tables)) {
		writeFieldsTable(doc, t, format.Tables[t])
	}
	if len(format.Variables) > 0 {
		doc.add(fieldsHeading, "  $variables:")
		writeFieldsSamples(doc, format.Variables)
	}
	writeFieldsNotes(doc, format)
	return true
}

// writeFieldsTables writes the tables of one log format matching table
// (any case, as a query's from clause), each followed by the $variables
// its lines filled, and reports whether any matched.
func writeFieldsTables(doc *fieldsDoc, name string, format catalogFormat, table string) bool {
	var tables []string
	for _, t := range slices.Sorted(maps.Keys(format.Tables)) {
		if strings.EqualFold(t, table) {
			tables = append(tables, t)
		}
	}
	if len(tables) == 0 {
		return false
	}
	doc.add(fieldsHeading, "log format %s:", displayText(name))
	for _, t := range tables {
		writeFieldsTable(doc, t, format.Tables[t])
		if variables := format.variablesOf(t); len(variables) > 0 {
			doc.add(fieldsHeading, "  $variables of table %s:", displayText(t))
			writeFieldsSamples(doc, variables)
		}
	}
	writeFieldsNotes(doc, format)
	return true
}

// writeFieldsTable writes the heading and fields of one table.
func writeFieldsTable(doc *fieldsDoc, table string, fields map[string][]string) {
	if table == "" {
		doc.add(fieldsHeading, "  fields:")
	} else {
		doc.add(fieldsHeading, "  table %s:", displayText(table))
	}
	writeFieldsSamples(doc, fields)
}

// writeFieldsNotes writes the notes of the custom probes of a format.
func writeFieldsNotes(doc *fieldsDoc, format catalogFormat) {
	for _, note := range format.Notes {
		doc.add(fieldsPlain, "  note: %s", displayText(note))
	}
}

// writeFieldsSamples writes one line per name, sorted, with its sample
// values quoted, the values aligned in one column.
func writeFieldsSamples(doc *fieldsDoc, samples map[string][]string) {
	names := slices.Sorted(maps.Keys(samples))
	width := 0
	for _, name := range names {
		// The names are padded by runes, so the width is counted in runes.
		width = min(max(width, utf8.RuneCountInString(displayText(name))), fieldsMaxNameWidth)
	}
	for _, name := range names {
		values := make([]string, len(samples[name]))
		for i, value := range samples[name] {
			values[i] = strconv.Quote(value)
		}
		shown := displayText(name)
		line := fieldsLine{kind: fieldsSample, text: fieldsSampleIndent + shown}
		if len(values) > 0 {
			line.pad = strings.Repeat(" ", max(width-utf8.RuneCountInString(shown), 0))
			line.values = strings.Join(values, ", ")
		}
		doc.lines = append(doc.lines, line)
	}
}

// completeFieldsTable completes the table argument of ":fields " from the
// field catalog's table names (ignoring case, as :fields matches them). It
// reports false for any other line, which completeInteractive then handles.
// Names :fields would show escaped or that contain a blank are not offered.
// An empty catalog lists nothing but fieldsTableHint, or the no-files hint
// for a session without files (catalogEmptyHint).
func completeFieldsTable(line string, pos int, catalog func() catalogSnapshot) (completion, bool) {
	before, after := line[:pos], line[pos:]
	start := strings.LastIndexAny(before, " \t") + 1
	prefix, word := before[:start], before[start:]
	// prefix ends with the blank before word, so ":fieldsx" never matches.
	if strings.TrimSpace(prefix) != ":fields" || start == 0 || strings.ContainsAny(word, `'"\`) {
		return completion{}, false
	}
	unchanged := completion{line: line, pos: pos}
	snap := catalog()
	if snap.empty() {
		unchanged.hint = catalogEmptyHint(snap, fieldsTableHint)
		return unchanged, true
	}
	var candidates []string
	for _, table := range catalogTables(snap) {
		if displayText(table) == table && !strings.ContainsAny(table, " \t") &&
			strings.HasPrefix(strings.ToUpper(table), strings.ToUpper(word)) {
			candidates = append(candidates, table)
		}
	}
	result := applyCompletion(unchanged, prefix, word, after, candidates)
	result.kind = candidateTable
	return result, true
}

// catalogTables returns the distinct non-empty table names of every log
// format of snap, sorted.
func catalogTables(snap catalogSnapshot) []string {
	var tables []string
	for _, format := range snap.Formats {
		for table := range format.Tables {
			if table != "" && !slices.Contains(tables, table) {
				tables = append(tables, table)
			}
		}
	}
	slices.Sort(tables)
	return tables
}

// displayText returns s as is when it is printable text without quotes or
// backslashes, and Go-quoted otherwise, so that text from a server cannot
// write control sequences to the terminal and an odd name stays visible.
func displayText(s string) string {
	if quoted := strconv.Quote(s); s != "" && quoted[1:len(quoted)-1] == s {
		return s
	}
	return strconv.Quote(s)
}

// displayList joins the displayText of every name with ", ", "none" for no
// name.
func displayList(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	shown := make([]string, len(names))
	for i, name := range names {
		shown[i] = displayText(name)
	}
	return strings.Join(shown, ", ")
}

package clients

import (
	"flag"
	"fmt"
	"slices"
	"strings"

	"github.com/mimecast/dtail/internal/config"
)

// interactiveCommands are the control commands Tab completes on the first
// word in every mode. :reload takes flags and :fields an optional table, so
// they complete with a trailing blank; interactiveCommandsFor adds :query
// and :tail where the mode has them.
var interactiveCommands = []string{":reload ", ":show", ":fields ", ":probe", ":help", ":quit"}

// interactiveCommandsFor returns the control commands of the mode of args:
// interactiveCommands, with :query in dtail and dmap and :tail in dtail
// (hasQueryCommand, hasTailCommand). Both complete with a trailing blank,
// as the argument usually follows.
func interactiveCommandsFor(args config.Args) []string {
	commands := slices.Clone(interactiveCommands)
	if hasQueryCommand(args) {
		commands = append(commands, queryCommand+" ")
	}
	if hasTailCommand(args) {
		commands = append(commands, tailCommand+" ")
	}
	return commands
}

// interactiveValueFlags maps the :reload flags whose current value Tab
// inserts (after the flag and a blank) to that value.
var interactiveValueFlags = map[string]func(config.Args) string{
	"grep":  func(a config.Args) string { return a.RegexStr },
	"regex": func(a config.Args) string { return a.RegexStr },
	"query": func(a config.Args) string { return a.QueryStr },
	"files": func(a config.Args) string { return a.What },
}

// completion is the result of one Tab press: the edited line and cursor, or
// the candidates to list when the word could not be extended.
type completion struct {
	line       string
	pos        int
	changed    bool
	candidates []string
	// hint is a line to show with the candidates (or alone), e.g. that the
	// field catalog is still empty.
	hint string
	// header is a line to show above the candidates, e.g. which server a
	// path listing came from.
	header string
	// kind is the kind of the candidates (candidateCommand by default); it
	// only decides the style of the listing.
	kind candidateKind
	// heading says what is expected at the cursor (e.g. "field to count")
	// and groups are the candidates of a query by what they are, labelled
	// (queryCompletion.groups): with groups the listing shows them under
	// their labels as bare names (interactiveStyle.listing), else the
	// candidates on one line.
	heading string
	groups  []candidateGroup
	// fetch is set by the --files completion (completeFilesValue) when the
	// path listing it needs is not cached: the listing request to send
	// before completing again.
	fetch string
	// probeTable is set by the --query completion (completeQueryValue) to
	// the from table typed in the query: the editor asks for a table probe
	// of it in the background (interactiveProber.requestTable), so that the
	// catalog gets that table's fields and $variables from a larger window.
	probeTable string
	// itemBlank is the length in bytes of the blank (escaped for an open
	// shell quote) that Tab inserted right before pos after a finished
	// query item (queryCompletion.afterItem with its lead), 0 when it inserted none: the
	// line editor lets a comma typed next replace it (commaAfterItemBlank).
	itemBlank int
}

// completeInteractive completes the word that ends at byte offset pos of a
// control line for the client arguments args:
//
//   - the first word: the control commands (:reload, :show, :fields,
//     :probe, :help, :quit, and :query and :tail where the mode has them);
//   - the word after ":fields": the tables of the field catalog
//     (completeFieldsTable);
//   - a word starting with "-" after :reload: the flags :reload accepts in
//     the mode of args, as "--name " (and "--name=false" for boolean flags);
//   - an empty word right after --grep, --regex, --query or --files: their
//     current value, quoted for splitInteractiveArgs, so that a long query
//     is edited rather than typed again; any other empty word after :reload:
//     the flags;
//   - a word inside a --query value (quoted, escaped, or --query=...): the
//     query language and the field catalog (completeQueryValue);
//   - a word of the query after ":query ": the same, inserted without
//     shell quoting (completeQueryCommand).
//
// A single candidate replaces the word, several extend it by their common
// prefix; when that adds nothing, they are returned to be listed. Outside a
// --query value, words containing quotes or backslashes, and lines whose
// earlier words do not tokenize (e.g. an open quote), are left alone. The
// field catalog (catalog) is only read for a --query value and a :fields
// table. A word inside a started --files value is completed from path
// listings by completeFilesValue, which the line editor tries first
// (completeEditorLine).
func completeInteractive(line string, pos int, args config.Args, catalog func() catalogSnapshot) completion {
	unchanged := completion{line: line, pos: pos}
	if pos < 0 || pos > len(line) {
		return unchanged
	}
	if result, ok := completeQueryValue(line, pos, args, catalog); ok {
		return result
	}
	if result, ok := completeQueryCommand(unchanged, line[:pos], line[pos:], args, catalog); ok {
		return result
	}
	if result, ok := completeFieldsTable(line, pos, catalog); ok {
		return result
	}
	return completePlainWord(unchanged, line[:pos], line[pos:], args)
}

// completePlainWord completes the plain word at the end of before (the
// line up to the cursor, after is the rest): a command as the first word,
// otherwise a word after :reload (reloadWordCandidates). A word with a
// quote or backslash, or after a blank escaped by a backslash (`foo\ `,
// part of the value), is no plain word and left alone.
func completePlainWord(unchanged completion, before, after string, args config.Args) completion {
	start := strings.LastIndexAny(before, " \t") + 1
	prefix, word := before[:start], before[start:]
	if strings.ContainsAny(word, `'"\`) || (start > 0 && escapedAt(before, start-1)) {
		return unchanged
	}
	if strings.TrimSpace(prefix) == "" {
		return applyCompletion(unchanged, prefix, word, after, matching(interactiveCommandsFor(args), word))
	}
	candidates, hint, ok := reloadWordCandidates(prefix, word, args)
	if !ok {
		return unchanged
	}
	result := applyCompletion(unchanged, prefix, word, after, candidates)
	result.hint = hint
	return result
}

// reloadWordCandidates returns the candidates for word after the earlier
// words prefix of a :reload line: the flags for a word starting with "-";
// for an empty word right after a flag that takes a value its current
// value (--grep, --regex, --query, --files) if it has one, otherwise
// nothing (a number is expected, or an empty value would be rejected);
// the flags for any other empty word. A current value that can not be put
// on the edit line (unprintableValueHint) gives no candidate but that hint.
// It reports false for a line that is no :reload or whose earlier words do
// not tokenize.
func reloadWordCandidates(prefix, word string, args config.Args) (candidates []string, hint string, ok bool) {
	rest, ok := strings.CutPrefix(strings.TrimLeft(prefix, " \t"), ":reload")
	if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
		return nil, "", false
	}
	tokens, err := splitInteractiveArgs(rest)
	if err != nil {
		return nil, "", false
	}
	switch {
	case strings.HasPrefix(word, "-"):
		return matching(interactiveReloadFlags(args), "--"+strings.TrimLeft(word, "-")), "", true
	case word != "":
		return nil, "", true
	case len(tokens) > 0 && flagTakesValue(tokens[len(tokens)-1], args):
		candidates, hint = currentValueCandidate(tokens[len(tokens)-1], args)
		return candidates, hint, true
	}
	return interactiveReloadFlags(args), "", true
}

// unprintableValueHint is shown instead of the current value of a :reload
// flag when that value holds a control or format character (e.g. a tab or
// line break from `--regex $'a\tb'` on the command line).
const unprintableValueHint = "(the current %s value holds a control or format character, " +
	"which the edit line can not show: type the new value)"

// currentValueCandidate returns the current value of the value flag token
// names, quoted for the :reload tokenizer (quoteInteractiveArg), as the one
// candidate; none for a flag without a value to insert. A value holding a
// control or format character (hasHiddenRune) is not inserted: the :reload
// tokenizer has no escape for it, so it could only go on the line raw,
// where the Terminal writes it as it is (a tab moves the cursor to the next
// tab stop, a line feed to the next row) and counts one column for it, so
// that every later cursor move and redraw of the line is off. The hint
// says so instead.
func currentValueCandidate(token string, args config.Args) (candidates []string, hint string) {
	value, ok := interactiveFlagValue(token, args)
	if !ok || value == "" {
		return nil, ""
	}
	if hasHiddenRune(value) {
		return nil, fmt.Sprintf(unprintableValueHint, "--"+strings.TrimLeft(token, "-"))
	}
	return []string{quoteInteractiveArg(value)}, ""
}

// applyCompletion replaces word by the only candidate or by the common
// prefix of several, or returns the candidates for listing.
func applyCompletion(unchanged completion, prefix, word, after string, candidates []string) completion {
	replacement := ""
	switch len(candidates) {
	case 0:
		return unchanged
	case 1:
		replacement = candidates[0]
	default:
		replacement = commonPrefix(candidates)
		if len(replacement) <= len(word) {
			unchanged.candidates = candidates
			return unchanged
		}
	}
	line := prefix + replacement + after
	return completion{line: line, pos: len(prefix) + len(replacement), changed: line != unchanged.line}
}

// interactiveReloadFlags returns the :reload flags of the mode of args as
// completion candidates, taken from the flag set the parser uses
// (newInteractiveReloadFlagSet), so completion never offers a flag the mode
// rejects. Value flags complete with a trailing blank; boolean flags also
// as "--name=false". A query session's --invert is left out: it is bound
// only to reject it with a reason.
func interactiveReloadFlags(args config.Args) []string {
	scratch := args
	fs, _, err := newInteractiveReloadFlagSet(args, &scratch)
	if err != nil {
		return nil
	}
	var flags []string
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name == "invert" && isInteractiveQueryMode(args) {
			return
		}
		flags = append(flags, "--"+f.Name+" ")
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			flags = append(flags, "--"+f.Name+"=false")
		}
	})
	slices.Sort(flags)
	return flags
}

// interactiveFlagValue returns the current value of the value flag token
// names (--grep, --regex, --query or --files, with one or two dashes and no
// "=value"), if it is one the mode of args accepts.
func interactiveFlagValue(token string, args config.Args) (string, bool) {
	name := strings.TrimLeft(token, "-")
	value, ok := interactiveValueFlags[name]
	if !ok || name == token || strings.Contains(name, "=") ||
		!slices.Contains(interactiveReloadFlags(args), "--"+name+" ") {
		return "", false
	}
	return value(args), true
}

// flagTakesValue reports whether token is a :reload flag of the mode of args
// that takes its value as the next argument: a non-boolean flag given
// without "=value".
func flagTakesValue(token string, args config.Args) bool {
	name := strings.TrimLeft(token, "-")
	if name == token || name == "" || strings.Contains(name, "=") {
		return false
	}
	scratch := args
	fs, _, err := newInteractiveReloadFlagSet(args, &scratch)
	if err != nil {
		return false
	}
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !b.IsBoolFlag()
}

// escapedAt reports whether the byte at i is escaped by an odd number of
// backslashes right before it.
func escapedAt(s string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

// quoteInteractiveArg quotes value so that splitInteractiveArgs returns it
// unchanged as one argument: as is when it consists of characters the
// tokenizer never treats specially, otherwise in single quotes, an
// embedded single quote closing the quotes, adding a backslash-escaped quote
// and opening them again (POSIX shell style). Control characters are kept
// raw inside the quotes (the tokenizer has no escape for them), so a caller
// that puts the result on the edit line must not pass a value holding one
// (currentValueCandidate).
func quoteInteractiveArg(value string) string {
	if value != "" && strings.Trim(value,
		"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./,:@%+=-") == "" {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func matching(candidates []string, word string) []string {
	var out []string
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, word) {
			out = append(out, candidate)
		}
	}
	return out
}

func commonPrefix(words []string) string {
	prefix := words[0]
	for _, w := range words[1:] {
		for !strings.HasPrefix(w, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	return prefix
}

package clients

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/protocol"
)

// filesFlagNames are the spellings of the --files flag a value belongs to.
var filesFlagNames = []string{"--files", "-files"}

// pathCompletionSource is what the --files completion reads: the cached
// path listings (fieldCatalog.findListing, with the current time bound) and
// the number of connections, as a listing is one server's view.
type pathCompletionSource struct {
	find    func(dir, base string) listingMatch
	servers int
}

// completeFilesValue completes the word at the cursor when it is inside the
// value of :reload --files (after "--files " in a started value, quoted or
// not, or in "--files=..."), in a mode whose :reload accepts --files. It
// reports false elsewhere, also where the value has not started and the
// session reads files (then the current files are inserted,
// reloadWordCandidates); in a session that reads none, a value not started
// is completed like a started empty one (filesValueAt).
//
// The value is split like session.splitFiles does: at commas, blanks
// around an entry trimmed, so the last entry is completed. Its directory
// part stays as typed and the base name after its last "/" (after
// "journal:" for journal units) is completed from the cached listing of
// that directory (findListing): directories with a trailing "/", files and
// units as they are. Names that could not be read as a --files entry (with
// a comma, blanks around them, or control characters) are not offered.
// Like completeQueryValue, the candidates are matched and their common
// prefix is taken on the decoded text, and only the replacement is escaped
// for the shell quote of the value (escapeForQuote), so a name with blanks
// is inserted quoted for its context; a base name whose bytes were not all
// read in that quote is left alone. When the cache lacks the listing, the
// result's fetch names the listing request to send (the caller waits for
// it briefly and completes again), with the candidates of a truncated
// listing, if any, meanwhile.
func completeFilesValue(line string, pos int, args config.Args, source pathCompletionSource) (completion, bool) {
	before, after := line[:pos], line[pos:]
	rest, ok := reloadArguments(before)
	if !ok || !slices.Contains(interactiveReloadFlags(args), "--files ") {
		return completion{}, false
	}
	parsed := splitInteractiveArgsPartial(rest)
	value, valueStart, ok := filesValueAt(parsed, args.What)
	if !ok {
		return completion{}, false
	}
	unchanged := completion{line: line, pos: pos}
	entryStart := lastFilesEntryStart(value)
	dir, base := listingDirKey(value[entryStart:])
	wordStart := valueStart + len(value) - len(base)
	if !parsed.suffixInQuote(wordStart) {
		return unchanged, true
	}
	match := source.find(dir, base)
	candidates := pathCandidates(match, base)
	result := unchanged
	result.kind = candidatePath
	if replacement, listed := decodedReplacement(base, candidates); replacement != "" {
		rawStart := len(before) - len(rest) + parsed.rawOffset(wordStart, len(rest))
		result.line = before[:rawStart] + escapeForQuote(replacement, parsed.quote) + after
		result.pos = len(result.line) - len(after)
		result.changed = result.line != line
	} else {
		result.candidates = listed
		result.header = pathListingHeader(match, listed, source.servers)
	}
	result.hint = pathListingHint(match)
	if !match.complete {
		result.fetch = match.fetch
	}
	return result, true
}

// reloadArguments returns the text after ":reload" of a control line typed
// up to the cursor, if the line is a :reload with at least one blank after
// the command.
func reloadArguments(before string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimLeft(before, " \t"), ":reload")
	if !ok || rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return "", false
	}
	return rest, true
}

// flagValueOf returns the value of the flag (one of names) the cursor is
// in, decoded so far, and the offset in parsed.current where it starts:
// "--flag=value" in one token, or a started token right after the flag that
// does not start with "-" (a flag).
func flagValueOf(parsed partialArgs, names []string) (string, int, bool) {
	if !parsed.started || parsed.escaped {
		return "", 0, false
	}
	for _, name := range names {
		if value, ok := strings.CutPrefix(parsed.current, name+"="); ok {
			return value, len(name) + 1, true
		}
	}
	if n := len(parsed.tokens); n > 0 && slices.Contains(names, parsed.tokens[n-1]) &&
		!strings.HasPrefix(parsed.current, "-") {
		return parsed.current, 0, true
	}
	return "", 0, false
}

// filesValueAt returns the --files value the cursor is in and its offset
// in parsed.current, like flagValueOf, and also takes a value that has not
// started (the cursor right after "--files " or "-files ") as an empty
// one when the session reads no files (namesNoFile(current), its --files
// value): there is no current value to insert then, and the session's hint
// (sessionNoFilesText) promises that Tab lists paths, so the working
// directory is listed as after "--files=".
func filesValueAt(parsed partialArgs, current string) (string, int, bool) {
	if value, start, ok := flagValueOf(parsed, filesFlagNames); ok {
		return value, start, true
	}
	n := len(parsed.tokens)
	if parsed.started || parsed.escaped || n == 0 || !slices.Contains(filesFlagNames, parsed.tokens[n-1]) ||
		!namesNoFile(current) {
		return "", 0, false
	}
	return "", 0, true
}

// namesNoFile reports whether the --files value files names no file to
// insert again: every entry (split as session.splitFiles does) is empty or
// stdin ("-", probeStdin, which a serverless session without files gets).
func namesNoFile(files string) bool {
	for _, entry := range strings.Split(files, ",") {
		if entry = strings.TrimSpace(entry); entry != "" && entry != probeStdin {
			return false
		}
	}
	return true
}

// lastFilesEntryStart returns the offset in a --files value where its last
// comma-separated entry starts, after the blanks session.splitFiles trims.
func lastFilesEntryStart(value string) int {
	start := strings.LastIndexByte(value, ',') + 1
	return start + len(value[start:]) - len(strings.TrimLeftFunc(value[start:], unicode.IsSpace))
}

// pathCandidates returns the names of the listing of match that start with
// base and can be read as a --files entry: directories with a trailing "/".
func pathCandidates(match listingMatch, base string) []string {
	if !match.found {
		return nil
	}
	var candidates []string
	for _, entry := range match.listing.entries {
		if !strings.HasPrefix(entry.Name, base) || !readableFilesName(entry.Name) {
			continue
		}
		if entry.Dir {
			candidates = append(candidates, entry.Name+"/")
		} else {
			candidates = append(candidates, entry.Name)
		}
	}
	return candidates
}

// readableFilesName reports whether name is offered as part of a --files
// entry: not empty, no comma (the entry separator), no blank at either end
// (which splitFiles trims), no control character (which the line editor
// cannot take) and no format character (unicode.Cf, e.g. the bidirectional
// override U+202E or a zero-width space), which would print invisibly and
// could make a listed name look like another.
func readableFilesName(name string) bool {
	return name != "" && !strings.ContainsRune(name, ',') && strings.TrimSpace(name) == name &&
		!hasHiddenRune(name)
}

// hiddenRune reports whether r is a control, format, line or paragraph
// separator character.
func hiddenRune(r rune) bool {
	// Line and paragraph separators (Zl, Zp: U+2028, U+2029) move no
	// terminal cursor the way the line editor counts, like a control
	// character.
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
}

// hasHiddenRune reports whether s holds a control or format character
// (hiddenRune). Completion offers no server or log text holding one: the
// path completion (readableFilesName) and the query completion
// (queryCandidateSource.groups) both drop it.
func hasHiddenRune(s string) bool {
	return strings.IndexFunc(s, hiddenRune) >= 0
}

// pathListingHeader returns the line shown above listed candidates when
// several servers are connected: the listing is one server's view.
func pathListingHeader(match listingMatch, listed []string, servers int) string {
	if len(listed) == 0 || servers < 2 {
		return ""
	}
	return fmt.Sprintf("(files as listed by %s, one of %d servers)", match.listing.server, servers)
}

// pathListingHint returns the hint line of a listing: the reason the
// server gave instead of entries, or that a truncated listing may lack
// names. The reason is server text, shown through displayText so that it
// cannot write control sequences to the terminal.
func pathListingHint(match listingMatch) string {
	switch {
	case !match.found:
		return ""
	case match.listing.err != "" && len(match.listing.entries) == 0:
		return "(path listing: " + displayText(match.listing.err) + ")"
	case match.listing.truncated && match.complete:
		return fmt.Sprintf("(the listing is cut at %d names or the server's limits: type more of the name)",
			protocol.ProbeListMaxEntries)
	}
	return ""
}

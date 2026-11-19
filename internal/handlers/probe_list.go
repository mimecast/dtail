package handlers

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/protocol"
)

// The path listing of the probe command (protocol.ProbeRequest.List), for
// the clients' path completion, bounded by the session user's readfiles
// permissions:
//   - A file is offered only when the readfiles permission check of a read
//     command (prober.resolve, symlinks resolved first) allows it, so no
//     file name is offered that the user could not read.
//   - A directory is offered unless the rules show that nothing beneath its
//     resolved path is readable: no allow rule can match a path beneath it,
//     or one deny rule already matches the directory path itself (a
//     match only at its end, e.g. by "$", must also match sample paths
//     beneath it, so "!^/var/log/[^/]*$" does not hide /var/log;
//     sessionuser.PathRules.MayReadBeneath). This is an approximation:
//     a directory denied by a rule that does not match "<dir>/" itself
//     ("!^/var/log/secret/.+$", "!^/var/log/private/.*\.log$"), or only
//     by several deny rules together, is offered although nothing beneath
//     it is readable (the cases: sessionuser.PathRules). Such
//     directory names are revealed, but only names an allow rule covers;
//     a glob read's "no permission" versus "no file" answers reveal as
//     much.
//   - The listed directory itself must pass the same directory rule;
//     otherwise, and when it does not exist or can not be read, the answer
//     is an empty listing, so a listing tells none of these apart.
//   - The directory part of a prefix is made absolute and cleaned
//     lexically (filepath.Abs) before its symlinks are resolved, so a ".."
//     removes the component before it whether that exists, is a directory or
//     is forbidden: "/a/missing/../b/" lists /a/b/. Resolving the raw prefix
//     would need every component before a ".." to exist, an existence
//     oracle of forbidden directories. Cleaning can not widen what is
//     listed: every check is made on the cleaned path with its remaining
//     symlinks resolved, as a read's permission check resolves them, so
//     relative prefixes and symlinks get the same rule as any other path.
//     A read cleans its path lexically too (filepath.Clean in
//     readCommand.readGlob and probeRun.globPaths), so a listed name read
//     with the prefix as typed is the file listed. Symlinks are offered as
//     what they point to; nothing is listed recursively.
//   - Names starting with "." are offered only for a prefix whose base name
//     starts with "." too; names that are no valid UTF-8 are never offered.
//   - The work is bounded (probeListLimits: names read from the directory,
//     permission checks, journal units), by protocol.ProbeListMaxEntries
//     entries and by the probe's timeout; reaching a bound sets Truncated,
//     also in an answer without entries: the client takes a listing
//     without Truncated as complete and narrows it locally, so it must
//     fetch a narrower prefix when the names checked (e.g. the first
//     probeListMaxChecks in sorted order, all denied) were not all names.
//     Truncated can thus reveal that many names not offered share a prefix.
//     This is an accepted oracle: the time a listing takes reveals their
//     number more precisely (some µs per checked name), and a read's "no
//     permission" versus "no file" answers reveal single names. A timeout
//     during the scan is reported as such (Truncated and probe: timed out).
//     The scan keeps only the names the checks can reach (boundedNames), so
//     it holds at most 2*probeListMaxChecks names at a time, not
//     probeListMaxScan.
const (
	// probeListMaxScan is the number of directory entries a listing reads
	// at most, so a huge directory costs bounded memory and time.
	probeListMaxScan = 100_000
	// probeListReadBatch is the number of names read from a directory at
	// once, between checks of the probe's context.
	probeListReadBatch = 1024
	// probeListMaxChecks is the number of names (of those matching the
	// prefix) a listing checks the permissions of at most. The scan of a
	// directory holds at most twice as many names at a time.
	probeListMaxChecks = 5000
	// probeListMaxUnits is the number of journal units a listing reads from
	// journalctl at most.
	probeListMaxUnits = 10_000
)

// probeUnitLister lists the systemd units of the journal, at most max of
// them, and reports whether there were more (journal.ListUnits).
type probeUnitLister func(ctx context.Context, max int) ([]string, bool, error)

// listPathRules is what a listing asks the session user's compiled rules
// (sessionuser.PathRules).
type listPathRules interface {
	MayReadBeneath(dir string) bool
	MayAllowPrefix(prefix string) bool
}

// probeListLimits bounds the work of a path listing.
type probeListLimits struct {
	maxScan, maxChecks, maxUnits int
}

// listRun answers one path listing request.
type listRun struct {
	*prober
	answer *protocol.ProbeAnswer
	// rules are the session user's rules for this listing (compiled once
	// per user).
	rules listPathRules
}

// withDefaults fills the zero limits with probeListMaxScan,
// probeListMaxChecks and probeListMaxUnits.
func (l probeListLimits) withDefaults() probeListLimits {
	if l.maxScan <= 0 {
		l.maxScan = probeListMaxScan
	}
	if l.maxChecks <= 0 {
		l.maxChecks = probeListMaxChecks
	}
	if l.maxUnits <= 0 {
		l.maxUnits = probeListMaxUnits
	}
	return l
}

// runList answers the path listing request (request.List set). It never
// fails: whatever goes wrong is an entry of the answer's Errors or, for a
// directory, an empty listing.
func (p *prober) runList(ctx context.Context, request protocol.ProbeRequest) protocol.ProbeAnswer {
	answer := protocol.ProbeAnswer{ID: request.ID}
	r := &listRun{prober: p, answer: &answer, rules: p.pathRules()}
	switch {
	case len(request.Files) > 0 || request.Table != "" || !validListPrefix(request.List):
		r.addError(protocol.ProbeErrorInvalidRequest)
	case p.listDisabled:
		r.addError(protocol.ProbeErrorListDisabled)
	case fs.IsJournalSpec(request.List):
		r.listJournal(ctx, request.List)
	default:
		r.listDirectory(ctx, request.List)
	}
	return answer
}

// validListPrefix reports whether prefix may be listed: at most
// protocol.ProbeListMaxPrefixBytes of valid UTF-8 without a NUL byte.
func validListPrefix(prefix string) bool {
	return prefix != "" && len(prefix) <= protocol.ProbeListMaxPrefixBytes &&
		utf8.ValidString(prefix) && !strings.ContainsRune(prefix, 0)
}

// splitListPrefix splits prefix after its last '/' into the directory part
// (as sent, "" when there is no '/') and the base name the entries must
// start with.
func splitListPrefix(prefix string) (dirPart, base string) {
	i := strings.LastIndexByte(prefix, '/')
	return prefix[:i+1], prefix[i+1:]
}

func (r *listRun) addError(reason string) {
	r.answer.Errors = append(r.answer.Errors, protocol.ProbeError{Error: reason})
}

// listDirectory lists the entries of the directory of prefix. See the
// comment at the top of the file for what it offers.
func (r *listRun) listDirectory(ctx context.Context, prefix string) {
	dirPart, base := splitListPrefix(prefix)
	r.answer.Dir = dirPart
	dir, ok := r.listableDir(dirPart)
	if !ok {
		return
	}
	names, truncated := r.scanDir(ctx, dir, base)
	r.answer.Truncated = truncated
	if ctx.Err() != nil {
		// The scan stopped at the probe's timeout, maybe before any name
		// matched: report it, so the answer is not taken as an empty but
		// complete listing (permittedEntries reports it only for a name it
		// was about to check).
		r.addError(protocol.ProbeErrorTimeout)
		r.answer.Truncated = true
		return
	}
	r.permittedEntries(ctx, names, func(name string) (protocol.ProbeEntry, bool) {
		return r.dirEntry(dir, name)
	})
}

// listableDir resolves the directory part of a prefix ("" is the working
// directory) to an absolute path without symlinks and reports whether its
// entries may be listed: it exists and the permissions may allow some path
// beneath it. The part is made absolute and cleaned lexically first, so no
// ".." is left for EvalSymlinks to walk through (see the top of the file);
// the permission check is made on the result with every symlink resolved.
func (r *listRun) listableDir(dirPart string) (string, bool) {
	if dirPart == "" {
		dirPart = "."
	}
	dir, err := filepath.Abs(dirPart)
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		r.logger.Debug(r.logContext, "Probe: unable to resolve directory to list", dirPart, err)
		return "", false
	}
	if !r.rules.MayReadBeneath(dir) {
		r.logger.Info(r.logContext, "Probe: no permission to list directory", dir)
		return "", false
	}
	return dir, true
}

// scanDir returns the sorted names in dir that start with base, without
// hidden names unless base starts with "." and without names that are no
// valid UTF-8, at most the first listLimits.maxChecks of them in sorted
// order (the only ones permittedEntries would check). It reads at most
// listLimits.maxScan names and reports whether it stopped before the end of
// the directory or dropped names past the first listLimits.maxChecks. A
// directory that can not be opened or read is logged at debug level only:
// any user reaches these lines with Tab, e.g. for a directory the rules
// offer but the OS does not let dserver read.
func (r *listRun) scanDir(ctx context.Context, dir, base string) ([]string, bool) {
	f, err := os.Open(dir)
	if err != nil {
		r.logger.Debug(r.logContext, "Probe: unable to list directory", dir, err)
		return nil, false
	}
	defer func() { _ = f.Close() }()
	showHidden := strings.HasPrefix(base, ".")
	names := boundedNames{keep: r.listLimits.maxChecks}
	for scanned := 0; ; {
		if scanned >= r.listLimits.maxScan || ctx.Err() != nil {
			result, _ := names.sorted()
			return result, true
		}
		batch, err := f.Readdirnames(min(probeListReadBatch, r.listLimits.maxScan-scanned))
		scanned += len(batch)
		for _, name := range batch {
			if strings.HasPrefix(name, base) && (showHidden || !strings.HasPrefix(name, ".")) &&
				utf8.ValidString(name) {
				names.add(name)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				r.logger.Debug(r.logContext, "Probe: unable to list directory", dir, err)
			}
			return names.sorted()
		}
	}
}

// boundedNames collects the first keep names, in sorted order, of those
// added, holding at most 2*keep of them at a time: whenever it holds more,
// it sorts them and drops all but the first keep. So a directory with up to
// probeListMaxScan matching names costs the memory of 2*probeListMaxChecks
// names, not of all of them, at an amortized O(log keep) per name.
type boundedNames struct {
	keep    int
	names   []string
	dropped bool
}

func (b *boundedNames) add(name string) {
	b.names = append(b.names, name)
	if len(b.names) > 2*b.keep {
		b.trim()
	}
}

// sorted returns the first keep names in sorted order and whether any
// added name was dropped.
func (b *boundedNames) sorted() ([]string, bool) {
	b.trim()
	return b.names, b.dropped
}

// trim sorts the names and drops all but the first keep.
func (b *boundedNames) trim() {
	slices.Sort(b.names)
	if len(b.names) > b.keep {
		clear(b.names[b.keep:])
		b.names = b.names[:b.keep]
		b.dropped = true
	}
}

func sortedNames(names []string) []string {
	slices.Sort(names)
	return names
}

// dirEntry returns the entry of name in the resolved directory dir and
// whether to offer it: a directory (also through a symlink) beneath whose
// resolved path the permissions may allow something, or a regular file the
// readfiles check allows. Anything else (sockets, devices, dangling or
// looping symlinks) is never offered.
func (r *listRun) dirEntry(dir, name string) (protocol.ProbeEntry, bool) {
	path := filepath.Join(dir, name)
	info, err := os.Stat(path)
	if err != nil {
		return protocol.ProbeEntry{}, false
	}
	switch {
	case info.IsDir():
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return protocol.ProbeEntry{}, false
		}
		return protocol.ProbeEntry{Name: name, Dir: true}, r.rules.MayReadBeneath(resolved)
	case info.Mode().IsRegular():
		_, err := r.resolve(path)
		return protocol.ProbeEntry{Name: name}, err == nil
	}
	return protocol.ProbeEntry{}, false
}

// listJournal lists the systemd units whose journal: target the user may
// read and whose names start with the rest of prefix. It needs the
// journal capability, as a journal read does. When no allow rule of the
// user can match a target starting with prefix, it answers an empty
// listing without running journalctl. The units come from listUnits, in
// dserver the unit list shared by all sessions for up to a minute
// (journal.UnitCache); the permission filtering here is per request.
func (r *listRun) listJournal(ctx context.Context, prefix string) {
	r.answer.Dir = fs.JournalSpecPrefix
	if !r.journalSupported {
		r.addError(protocol.ProbeErrorJournalUnsupported)
		return
	}
	if !r.rules.MayAllowPrefix(prefix) {
		return
	}
	units, truncated, err := r.listUnits(ctx, r.listLimits.maxUnits)
	if err != nil {
		r.logger.Warn(r.logContext, "Probe: unable to list journal units", err)
		if ctx.Err() != nil {
			r.addError(protocol.ProbeErrorTimeout)
		} else {
			r.addError(protocol.ProbeErrorListFailed)
		}
		return
	}
	r.answer.Truncated = truncated
	names := unitsWithPrefix(units, strings.TrimPrefix(prefix, fs.JournalSpecPrefix))
	r.permittedEntries(ctx, names, func(unit string) (protocol.ProbeEntry, bool) {
		_, err := r.resolve(fs.JournalSpecPrefix + unit)
		return protocol.ProbeEntry{Name: unit}, err == nil
	})
}

// unitsWithPrefix returns the units starting with base that are valid
// UTF-8, sorted, each once.
func unitsWithPrefix(units []string, base string) []string {
	var names []string
	for _, unit := range units {
		if strings.HasPrefix(unit, base) && utf8.ValidString(unit) {
			names = append(names, unit)
		}
	}
	return slices.Compact(sortedNames(names))
}

// permittedEntries appends the entries of the sorted names that check
// offers, at most protocol.ProbeListMaxEntries of them, checking at most
// listLimits.maxChecks names. It sets Truncated when it stops before the last
// name, and reports a timeout when the probe's context ended first.
func (r *listRun) permittedEntries(ctx context.Context, names []string,
	check func(string) (protocol.ProbeEntry, bool)) {

	for i, name := range names {
		if len(r.answer.Entries) == protocol.ProbeListMaxEntries || i == r.listLimits.maxChecks {
			r.answer.Truncated = true
			return
		}
		if ctx.Err() != nil {
			r.answer.Truncated = true
			r.addError(protocol.ProbeErrorTimeout)
			return
		}
		if entry, ok := check(name); ok {
			r.answer.Entries = append(r.answer.Entries, entry)
		}
	}
}

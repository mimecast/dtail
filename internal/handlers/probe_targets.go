package handlers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/protocol"
	user "github.com/mimecast/dtail/internal/sessionuser"
)

// probeTimeout bounds the sampling of one probe (journalctl included):
// files not sampled by then are reported with protocol.ProbeErrorTimeout.
// The other steps are bounded by size, not by this timeout: one glob and at
// most MaxGlobTargets permission checks per entry, and the log format probes
// of at most protocol.ProbeMaxFiles bounded samples.
const probeTimeout = 10 * time.Second

// probeMaxErrorFileBytes is the length the request entry an error names is
// cut to, so that a long entry cannot crowd the answer.
const probeMaxErrorFileBytes = 256

// prober runs probe requests. It holds what a probe needs of its session as
// plain values and functions, so it is independent of the handler: resolve
// is the session's read permission check, the one every read command goes
// through, logging about the path at debug level only
// (ServerHandler.probeReadTarget: a probe checks many paths every round of
// every interactive session and logs one summary line instead), and sample
// reads a validated
// target (probe.Sample). The path listing (probe_list.go) also takes
// the session user's rules once per listing (pathRules,
// sessionuser.User.PathRules, compiled once per user) to ask about
// directories, and lists the
// journal units with listUnits (the dserver's journal.UnitCache, or
// journal.ListUnits without one), within listLimits;
// listDisabled is Server.PathCompletionDisable.
type prober struct {
	resolve          func(path string) (fs.ValidatedReadTarget, error)
	pathRules        func() listPathRules
	sample           probeSampler
	listUnits        probeUnitLister
	listLimits       probeListLimits
	listDisabled     bool
	journalSupported bool
	maxGlobTargets   int
	maxLineLength    int
	hostname         string
	defaultLogFormat string
	logger           logging.Logger
	logContext       any
}

// probeSampler returns a bounded sample of a validated read target.
type probeSampler func(ctx context.Context, target fs.ValidatedReadTarget, path string,
	options fs.SampleOptions) (fs.Sample, error)

// probeTarget is one file (or journal target) a probe samples.
type probeTarget struct {
	// entry is the request entry that named the target.
	entry  string
	path   string
	target fs.ValidatedReadTarget
}

// probeRun collects the errors of one probe request, each (entry, reason)
// pair once.
type probeRun struct {
	*prober
	errors []protocol.ProbeError
	seen   map[protocol.ProbeError]struct{}
}

func newProbeRun(p *prober) *probeRun {
	return &probeRun{prober: p, seen: make(map[protocol.ProbeError]struct{})}
}

// addError records reason for entry (a request entry or a log format name).
func (r *probeRun) addError(entry, reason string) {
	entry = cappedProbeEntry(entry)
	probeErr := protocol.ProbeError{File: entry, Error: reason}
	if _, ok := r.seen[probeErr]; ok {
		return
	}
	r.seen[probeErr] = struct{}{}
	r.errors = append(r.errors, probeErr)
}

// entryFailed records reason for the request entry and logs it once at
// warn level with the number of the entry's paths (glob matches) it holds
// for. The permission checks of a probe log at debug level only
// (ServerHandler.probeReadTarget), so this line is what tells the server
// log which entries a probe could not read, as a read logs its failures:
// at most one line per entry and reason, not one per glob match.
func (r *probeRun) entryFailed(entry, reason string, matches int) {
	r.addError(entry, reason)
	r.logger.Warn(r.logContext, "Probe: unable to read entry", cappedProbeEntry(entry),
		reason, "matches", matches)
}

// cappedProbeEntry cuts entry to probeMaxErrorFileBytes, so that a long
// entry can crowd neither the answer nor the server log.
func cappedProbeEntry(entry string) string {
	if len(entry) > probeMaxErrorFileBytes {
		return strings.ToValidUTF8(entry[:probeMaxErrorFileBytes], "")
	}
	return entry
}

// resolveTargets returns the targets of the request's entries, at most
// protocol.ProbeMaxFiles over all entries, in the order of the entries and,
// within a glob, of its sorted matches. Every entry goes the way of a read
// command's path: protocol.DecodeReadPath (readCommand.parsePath), then a
// journal target or filepath.Clean and filepath.Glob capped at
// MaxGlobTargets (readCommand.readGlob), then the readfiles permission check
// of every match (readCommand.readFileIfPermissions). Unlike a read, a probe
// globs once, without retries, and never reads stdin ("-"). Entries beyond
// protocol.ProbeMaxFiles are not looked at, so a request of many entries
// cannot make the server glob and check paths without bound.
func (r *probeRun) resolveTargets(entries []string) []probeTarget {
	if len(entries) > protocol.ProbeMaxFiles {
		entries = entries[:protocol.ProbeMaxFiles]
		r.addError("", protocol.ProbeErrorTooManyFiles)
	}
	var targets []probeTarget
	for _, entry := range entries {
		paths, ok := r.entryPaths(entry)
		if !ok {
			continue
		}
		var full bool
		targets, full = r.appendTargets(targets, entry, paths)
		if full {
			r.addError("", protocol.ProbeErrorTooManyFiles)
			break
		}
	}
	return targets
}

// entryPaths decodes entry and returns its paths: the journal target itself,
// or the matches of the glob. It records why it returns none.
func (r *probeRun) entryPaths(entry string) ([]string, bool) {
	path, err := protocol.DecodeReadPath(entry)
	if err != nil {
		r.addError(entry, protocol.CommandFailureReadCommand)
		return nil, false
	}
	switch {
	case strings.TrimSpace(path) == "" || path == "-":
		r.addError(entry, protocol.ProbeErrorInvalidRequest)
		return nil, false
	case fs.IsJournalSpec(path):
		if !r.journalSupported {
			r.addError(entry, protocol.ProbeErrorJournalUnsupported)
			return nil, false
		}
		return []string{path}, true
	}
	return r.globPaths(entry, path)
}

// globPaths expands the glob path like readCommand.readGlob does in one
// attempt, capped at maxGlobTargets.
func (r *probeRun) globPaths(entry, glob string) ([]string, bool) {
	glob = filepath.Clean(glob)
	paths, err := filepath.Glob(glob)
	if err != nil {
		r.logger.Warn(r.logContext, "Probe: invalid glob", glob, err)
		r.addError(entry, protocol.CommandFailureReadCommand)
		return nil, false
	}
	if len(paths) == 0 {
		r.entryFailed(entry, protocol.CommandFailureNoFile, 0)
		return nil, false
	}
	if len(paths) > r.maxGlobTargets {
		r.logger.Warn(r.logContext, "Probe: glob expansion exceeded cap, truncating",
			"glob", glob, "matched", len(paths), "cap", r.maxGlobTargets)
		r.addError(entry, protocol.CommandFailureGlobCapped)
		paths = paths[:r.maxGlobTargets]
	}
	return paths, true
}

// appendTargets appends the paths of entry that the session may read to
// targets. A path resolving to a target already listed (e.g. "a.log" and
// "*.log", or a symlink and its target) is not added again, so a file is
// sampled once and takes one of the protocol.ProbeMaxFiles slots. It
// reports true when targets reached protocol.ProbeMaxFiles while paths were
// left, which are then dropped.
//
// The paths it can not read are logged per path at debug level only (a glob
// can match up to MaxGlobTargets denied files, every round) and per entry
// and reason, with their count, at warn level (entryFailed).
func (r *probeRun) appendTargets(targets []probeTarget, entry string, paths []string) ([]probeTarget, bool) {
	skipped := 0
	var failed failedMatches
	defer func() { failed.report(r, entry) }()
	for _, path := range paths {
		if len(targets) == protocol.ProbeMaxFiles {
			return targets, true
		}
		target, err := r.resolve(path)
		switch {
		case err == nil:
			if !hasProbeTarget(targets, target) {
				targets = append(targets, probeTarget{entry: entry, path: path, target: target})
			}
		case errors.Is(err, fs.ErrNotRegularFile):
			skipped++
		default:
			r.logger.Debug(r.logContext, "Probe: unable to read file", path, err)
			failed.add(probeResolveReason(err), 1)
		}
	}
	if skipped == len(paths) {
		failed.add(protocol.CommandFailureNoFile, skipped)
	}
	return targets, false
}

// failedMatches counts the paths of one entry that could not be read, per
// reason, in the order the reasons first occurred.
type failedMatches struct {
	reasons []string
	counts  map[string]int
}

// add counts n more paths failing with reason.
func (f *failedMatches) add(reason string, n int) {
	if f.counts == nil {
		f.counts = make(map[string]int)
	}
	if _, ok := f.counts[reason]; !ok {
		f.reasons = append(f.reasons, reason)
	}
	f.counts[reason] += n
}

// report records and logs every reason of entry once (entryFailed).
func (f *failedMatches) report(r *probeRun, entry string) {
	for _, reason := range f.reasons {
		r.entryFailed(entry, reason, f.counts[reason])
	}
}

// hasProbeTarget reports whether targets list target already, compared by
// kind and resolved path (symlinks evaluated by the permission check).
func hasProbeTarget(targets []probeTarget, target fs.ValidatedReadTarget) bool {
	for _, listed := range targets {
		if listed.target.Kind == target.Kind && listed.target.ResolvedPath() == target.ResolvedPath() {
			return true
		}
	}
	return false
}

// probeResolveReason maps an error of the read permission check to the
// reason a read command reports for it (readCommand.readFileIfPermissions).
func probeResolveReason(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return protocol.CommandFailureNoFile
	case errors.Is(err, user.ErrReadPermissionDenied):
		return protocol.CommandFailurePermission
	default:
		return protocol.CommandFailureReadingFile
	}
}

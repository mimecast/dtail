package handlers

// Tests of the probe command's path listing (probe_list.go): it offers only
// files the session user may read and directories beneath which the
// permissions may allow something, and reveals nothing else, whatever the
// prefix (symlinks, "..", relative paths, hidden names, missing and
// forbidden directories), within its bounds.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/mimecast/dtail/internal/protocol"
)

// probeListTree is a directory tree with files and directories the
// permissions of probeListPermissions allow and deny.
type probeListTree struct {
	root, logs, other string
}

// newProbeListTree builds the tree:
//
//	logs/app.log, app.log.1, .hidden.log   permitted
//	logs/secret.log                        denied by a deny rule
//	logs/sub/nested.log                    permitted
//	logs/empty/                            permitted beneath, empty
//	logs/private/p.log                     denied by a deny rule of the directory
//	logs/archive/a.log, archive/sub/b.log  denied by a deny rule of the form
//	                                       "!^<dir>/.*$" (doc/server-configuration.md)
//	logs/link-to-sub -> sub                a permitted directory
//	logs/link-to-other -> ../other         a directory outside the permissions
//	logs/link-allowed -> app.log           a permitted file
//	logs/link-outside -> ../other/x.log    a file outside the permissions
//	logs/dangling -> missing.log           points nowhere
//	logs/fifo                              no regular file
//	other/x.log, other/dir/                outside the permissions
//	other/link-to-logs -> ../logs          a permitted directory, but its name
//	                                       is in a directory outside them
func newProbeListTree(t *testing.T) probeListTree {
	t.Helper()
	root := realTempDir(t)
	tree := probeListTree{root: root, logs: filepath.Join(root, "logs"), other: filepath.Join(root, "other")}
	for _, name := range []string{"app.log", "app.log.1", ".hidden.log", "secret.log"} {
		writeProbeTestFile(t, tree.logs, name, "line")
	}
	writeProbeTestFile(t, filepath.Join(tree.logs, "sub"), "nested.log", "line")
	writeProbeTestFile(t, filepath.Join(tree.logs, "private"), "p.log", "line")
	writeProbeTestFile(t, filepath.Join(tree.logs, "archive"), "a.log", "line")
	writeProbeTestFile(t, filepath.Join(tree.logs, "archive", "sub"), "b.log", "line")
	writeProbeTestFile(t, tree.other, "x.log", "line")
	for _, dir := range []string{filepath.Join(tree.logs, "empty"), filepath.Join(tree.other, "dir")} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		"link-to-sub": "sub", "link-to-other": "../other", "link-allowed": "app.log",
		"link-outside": "../other/x.log", "dangling": "missing.log",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(tree.logs, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../logs", filepath.Join(tree.other, "link-to-logs")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(tree.logs, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	return tree
}

// permissions returns the rules of the tree: everything beneath logs/ but
// secret.log, private/ and archive/.
func (tree probeListTree) permissions() []string {
	logs := regexp.QuoteMeta(tree.logs)
	return []string{
		"readfiles:!^" + logs + "/archive/.*$",
		"readfiles:^" + logs + "/",
		"readfiles:!^" + logs + "/secret\\.log$",
		"readfiles:!^" + logs + "/private/",
	}
}

// probeList runs a path listing of prefix and returns its answer.
func probeList(t *testing.T, handler *ServerHandler, prefix string) protocol.ProbeAnswer {
	t.Helper()
	answer, _ := runProbe(t, handler, probeCommandFor(t, protocol.ProbeRequest{ID: "l1", List: prefix}))
	if answer.ID != "l1" {
		t.Fatalf("answer id = %q, want l1", answer.ID)
	}
	return answer
}

// entryNames returns the names of the answer's entries, directories with a
// trailing "/".
func entryNames(answer protocol.ProbeAnswer) []string {
	names := []string{}
	for _, entry := range answer.Entries {
		if entry.Dir {
			names = append(names, entry.Name+"/")
		} else {
			names = append(names, entry.Name)
		}
	}
	return names
}

func TestProbeListOffersOnlyPermittedPaths(t *testing.T) {
	tree := newProbeListTree(t)
	handler := newProbeTestHandler(t, tree.permissions(), nil)
	tests := []struct {
		name, prefix, dir string
		want              []string
	}{
		{name: "permitted directory", prefix: tree.logs + "/", dir: tree.logs + "/",
			want: []string{"app.log", "app.log.1", "empty/", "link-allowed", "link-to-sub/", "sub/"}},
		{name: "name prefix", prefix: tree.logs + "/app", dir: tree.logs + "/",
			want: []string{"app.log", "app.log.1"}},
		{name: "hidden names only for a hidden prefix", prefix: tree.logs + "/.", dir: tree.logs + "/",
			want: []string{".hidden.log"}},
		{name: "directory leading to permitted files", prefix: tree.root + "/", dir: tree.root + "/",
			want: []string{"logs/"}},
		{name: "ancestor directory", prefix: filepath.Dir(tree.root) + "/" + filepath.Base(tree.root)[:3],
			dir: filepath.Dir(tree.root) + "/", want: []string{filepath.Base(tree.root) + "/"}},
		{name: "denied file by name", prefix: tree.logs + "/secret", dir: tree.logs + "/", want: []string{}},
		{name: "denied directory by name", prefix: tree.logs + "/priv", dir: tree.logs + "/", want: []string{}},
		{name: "inside a denied directory", prefix: tree.logs + "/private/", dir: tree.logs + "/private/",
			want: []string{}},
		{name: "directory denied by a .*$ rule, by name", prefix: tree.logs + "/arch", dir: tree.logs + "/",
			want: []string{}},
		{name: "inside a directory denied by a .*$ rule", prefix: tree.logs + "/archive/",
			dir: tree.logs + "/archive/", want: []string{}},
		{name: "directory outside the permissions", prefix: tree.other + "/", dir: tree.other + "/",
			want: []string{}},
		{name: "symlinked directory outside the permissions", prefix: tree.logs + "/link-to-other/",
			dir: tree.logs + "/link-to-other/", want: []string{}},
		{name: "symlinked permitted directory", prefix: tree.logs + "/link-to-sub/",
			dir: tree.logs + "/link-to-sub/", want: []string{"nested.log"}},
		{name: "dot-dot out of the permissions", prefix: tree.logs + "/../other/",
			dir: tree.logs + "/../other/", want: []string{}},
		{name: "dot-dot into the permissions", prefix: tree.other + "/../logs/sub/n",
			dir: tree.other + "/../logs/sub/", want: []string{"nested.log"}},
		{name: "missing directory", prefix: tree.logs + "/missing/", dir: tree.logs + "/missing/",
			want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			answer := probeList(t, handler, tt.prefix)
			if got := entryNames(answer); !slices.Equal(got, tt.want) {
				t.Fatalf("entries of %q = %q, want %q", tt.prefix, got, tt.want)
			}
			if answer.Dir != tt.dir || answer.Truncated || len(answer.Errors) != 0 {
				t.Fatalf("answer of %q = %+v, want dir %q, no errors", tt.prefix, answer, tt.dir)
			}
		})
	}
}

// TestProbeListIsNoExistenceOracle checks that a directory outside the
// permissions, one that does not exist, an unreadable one and a forbidden
// one inside a permitted tree all get the same answer, so a listing tells
// nothing about what the user may not read.
func TestProbeListIsNoExistenceOracle(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lists directories without read permission")
	}
	tree := newProbeListTree(t)
	handler := newProbeTestHandler(t, tree.permissions(), nil)
	unreadable := filepath.Join(tree.logs, "unreadable")
	writeProbeTestFile(t, unreadable, "u.log", "line")
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })

	for _, dir := range []string{tree.other, filepath.Join(tree.other, "dir"), filepath.Join(tree.root, "nothing"),
		filepath.Join(tree.logs, "private"), filepath.Join(tree.logs, "archive", "sub"), unreadable, filepath.Join(tree.logs, "app.log")} {
		answer := probeList(t, handler, dir+"/")
		answer.Dir = ""
		if want := (protocol.ProbeAnswer{ID: "l1"}); !answerEqual(answer, want) {
			t.Fatalf("listing of %s = %+v, want an empty listing", dir, answer)
		}
	}
	checkDotDotIsNoExistenceOracle(t, handler, tree)
}

// checkDotDotIsNoExistenceOracle checks that a ".." in a prefix tells
// nothing about the components before it: whether the directory before the
// ".." is a forbidden one that exists, a missing one or a file, the listing
// is that of the lexically cleaned prefix (as filepath.Clean gives it).
func checkDotDotIsNoExistenceOracle(t *testing.T, handler *ServerHandler, tree probeListTree) {
	t.Helper()
	want := entryNames(probeList(t, handler, tree.logs+"/app"))
	if !slices.Equal(want, []string{"app.log", "app.log.1"}) {
		t.Fatalf("listing of %s/app = %q", tree.logs, want)
	}
	// The ".." of logs/link-to-other/.. removes the symlink, not its target.
	for _, dir := range []string{filepath.Join(tree.other, "dir"), filepath.Join(tree.other, "missing"),
		filepath.Join(tree.other, "x.log"), filepath.Join(tree.logs, "private"),
		filepath.Join(tree.logs, "link-to-other")} {
		prefix := dir + "/../../logs/app"
		answer := probeList(t, handler, prefix)
		if got := entryNames(answer); !slices.Equal(got, want) || answer.Truncated || len(answer.Errors) != 0 {
			t.Fatalf("listing of %s = %+v, want the entries %q", prefix, answer, want)
		}
		if wantDir := dir + "/../../logs/"; answer.Dir != wantDir {
			t.Fatalf("listing of %s: dir %q, want the directory part as sent %q", prefix, answer.Dir, wantDir)
		}
	}
	// Cleaning never lists more than the cleaned path's rules allow: the
	// kernel would resolve other/link-to-logs/.. to the root, beneath which
	// logs/ is permitted, but the cleaned path is other/, which is not.
	for _, prefix := range []string{tree.other + "/link-to-logs/../", tree.other + "/link-to-logs/../l",
		tree.logs + "/../other/"} {
		if got := entryNames(probeList(t, handler, prefix)); len(got) != 0 {
			t.Fatalf("listing of %s = %q, want nothing", prefix, got)
		}
	}
}

func answerEqual(a, b protocol.ProbeAnswer) bool {
	return fmt.Sprintf("%+v", a) == fmt.Sprintf("%+v", b)
}

// TestProbeListWithoutPermissions checks that a user whose permissions
// cover no path of the tree is offered nothing in it.
func TestProbeListWithoutPermissions(t *testing.T) {
	tree := newProbeListTree(t)
	for _, permissions := range [][]string{nil, {"readfiles:^/nothing-here/"}, {"readfiles:^journal:.*"}} {
		handler := newProbeTestHandler(t, permissions, nil)
		for _, prefix := range []string{tree.root + "/", tree.logs + "/", tree.logs + "/app", "/"} {
			if answer := probeList(t, handler, prefix); len(answer.Entries) != 0 {
				t.Fatalf("permissions %q, listing of %s = %q, want nothing", permissions, prefix,
					entryNames(answer))
			}
		}
	}
}

// TestProbeListRelativePrefix checks that a relative prefix is listed in
// dserver's working directory under the same rules as its absolute path.
func TestProbeListRelativePrefix(t *testing.T) {
	tree := newProbeListTree(t)
	handler := newProbeTestHandler(t, tree.permissions(), nil)
	t.Chdir(tree.root)

	answer := probeList(t, handler, "logs/app")
	if got := entryNames(answer); !slices.Equal(got, []string{"app.log", "app.log.1"}) || answer.Dir != "logs/" {
		t.Fatalf("relative listing = %q in %q", got, answer.Dir)
	}
	if got := entryNames(probeList(t, handler, "l")); !slices.Equal(got, []string{"logs/"}) {
		t.Fatalf("listing of the working directory = %q, want [logs/]", got)
	}
	if got := entryNames(probeList(t, handler, "./")); !slices.Equal(got, []string{"logs/"}) {
		t.Fatalf("listing of ./ = %q, want [logs/]", got)
	}
	if got := entryNames(probeList(t, handler, "other/")); len(got) != 0 {
		t.Fatalf("relative listing outside the permissions = %q", got)
	}
}

// TestProbeListCaps checks protocol.ProbeListMaxEntries and the answer size
// cap: the first entries in sorted order are kept and Truncated is set.
func TestProbeListCaps(t *testing.T) {
	dir := realTempDir(t)
	handler := newProbeTestHandler(t, []string{"readfiles:^" + regexp.QuoteMeta(dir) + "/"}, nil)
	long := strings.Repeat("n", 150)
	for i := range protocol.ProbeListMaxEntries + 20 {
		writeProbeTestFile(t, dir, fmt.Sprintf("f%04d.log", i), "line")
		writeProbeTestFile(t, dir, fmt.Sprintf("long%04d%s", i, long), "line")
	}

	answer := probeList(t, handler, dir+"/f")
	if len(answer.Entries) != protocol.ProbeListMaxEntries || !answer.Truncated {
		t.Fatalf("entries = %d, truncated = %v, want %d and truncated", len(answer.Entries),
			answer.Truncated, protocol.ProbeListMaxEntries)
	}
	if answer.Entries[0].Name != "f0000.log" || answer.Entries[len(answer.Entries)-1].Name != "f0499.log" {
		t.Fatalf("entries are not the first ones in order: %q ... %q", answer.Entries[0].Name,
			answer.Entries[len(answer.Entries)-1].Name)
	}

	_, frame := runProbe(t, handler, probeCommandFor(t, protocol.ProbeRequest{ID: "l2", List: dir + "/long"}))
	answer, err := protocol.DecodeProbeAnswer(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !answer.Truncated || len(answer.Entries) == 0 || len(answer.Entries) >= protocol.ProbeListMaxEntries {
		t.Fatalf("long names: %d entries, truncated %v", len(answer.Entries), answer.Truncated)
	}
	if answer.Entries[0].Name != "long0000"+long {
		t.Fatalf("long names: first entry %q", answer.Entries[0].Name)
	}
	if data := frame[strings.LastIndexByte(frame, ' ')+1:]; len(data) > 4*protocol.ProbeMaxAnswerBytes/3+4 {
		t.Fatalf("answer frame of %d bytes exceeds the cap", len(data))
	}
}

func TestProbeListRejectsInvalidRequests(t *testing.T) {
	dir := realTempDir(t)
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	for name, request := range map[string]protocol.ProbeRequest{
		"files and list": {ID: "l1", List: dir + "/", Files: []string{dir + "/x.log"}},
		"NUL byte":       {ID: "l1", List: dir + "/\x00x"},
		"too long":       {ID: "l1", List: "/" + strings.Repeat("a", protocol.ProbeListMaxPrefixBytes)},
	} {
		answer, _ := runProbe(t, handler, probeCommandFor(t, request))
		if len(answer.Entries) != 0 || !hasProbeError(answer, "", protocol.ProbeErrorInvalidRequest) {
			t.Fatalf("%s: answer %+v, want probe: invalid request", name, answer)
		}
	}
}

func TestProbeListDisabledByConfig(t *testing.T) {
	tree := newProbeListTree(t)
	handler := newProbeTestHandler(t, tree.permissions(), nil, protocol.CapabilityJournalV1)
	handler.serverCfg.PathCompletionDisable = true
	handler.probeUnitLister = func(context.Context, int) ([]string, bool, error) {
		t.Error("journal units listed although path listing is disabled")
		return nil, false, nil
	}
	for _, prefix := range []string{tree.logs + "/", "journal:"} {
		answer := probeList(t, handler, prefix)
		if len(answer.Entries) != 0 || !hasProbeError(answer, "", protocol.ProbeErrorListDisabled) {
			t.Fatalf("disabled listing of %s: %+v", prefix, answer)
		}
	}
	// A field probe still works.
	file := filepath.Join(tree.logs, "app.log")
	if answer, _ := probeFiles(t, handler, file); answer.FilesSampled != 1 {
		t.Fatalf("field probe with path listing disabled: %+v", answer)
	}
}

func TestProbeListJournalUnits(t *testing.T) {
	units := []string{"sshd.service", "app-web.service", "app-db.service", "cron.service", "app-web.service",
		"bad unit.service"}
	lister := func(_ context.Context, max int) ([]string, bool, error) {
		if max != probeListMaxUnits {
			t.Errorf("unit cap %d, want %d", max, probeListMaxUnits)
		}
		return units, false, nil
	}
	permissions := []string{`readfiles:^journal:app-.*\.service$`, `readfiles:!^journal:app-db\.`}

	withoutCapability := newProbeTestHandler(t, permissions, nil)
	withoutCapability.probeUnitLister = lister
	answer := probeList(t, withoutCapability, "journal:")
	if len(answer.Entries) != 0 || !hasProbeError(answer, "", protocol.ProbeErrorJournalUnsupported) {
		t.Fatalf("journal listing without capability: %+v", answer)
	}

	handler := newProbeTestHandler(t, permissions, nil, protocol.CapabilityJournalV1)
	handler.probeUnitLister = lister
	for prefix, want := range map[string][]string{
		"journal:":     {"app-web.service"},
		"journal:app-": {"app-web.service"},
		"journal:ssh":  {},
		"journal:x":    {},
	} {
		answer := probeList(t, handler, prefix)
		if got := entryNames(answer); !slices.Equal(got, want) || answer.Dir != "journal:" || len(answer.Errors) != 0 {
			t.Fatalf("listing of %s = %q (%+v), want %q", prefix, got, answer, want)
		}
	}

	handler.probeUnitLister = func(context.Context, int) ([]string, bool, error) {
		return nil, false, errors.New("journalctl failed")
	}
	if answer := probeList(t, handler, "journal:"); !hasProbeError(answer, "", protocol.ProbeErrorListFailed) {
		t.Fatalf("failing journalctl: %+v", answer)
	}
}

// TestProbeListJournalWithoutJournalRules checks that a user whose allow
// rules can match no target with the prefix gets an empty listing without
// journalctl being run.
func TestProbeListJournalWithoutJournalRules(t *testing.T) {
	notCalled := func(context.Context, int) ([]string, bool, error) {
		t.Error("journalctl run for a user whose rules match no journal target")
		return []string{"app-web.service"}, false, nil
	}
	for permissions, prefix := range map[string]string{
		"readfiles:^/var/log/":                       "journal:",
		`readfiles:^journal:app-.*\.service$`:        "journal:ssh",
		"readfiles:!^journal:":                       "journal:",
		`readfiles:^/var/log/.*\.log$|^/srv/data/.*`: "journal:app",
	} {
		handler := newProbeTestHandler(t, []string{permissions}, nil, protocol.CapabilityJournalV1)
		handler.probeUnitLister = notCalled
		answer := probeList(t, handler, prefix)
		if len(answer.Entries) != 0 || len(answer.Errors) != 0 || answer.Dir != "journal:" {
			t.Fatalf("rules %q, listing of %s: %+v, want an empty listing", permissions, prefix, answer)
		}
	}
}

// TestProbeListLimits checks the bounds of the directory scan and of the
// permission checks: a listing stops at them and sets Truncated.
func TestProbeListLimits(t *testing.T) {
	dir := realTempDir(t)
	for i := range 40 {
		writeProbeTestFile(t, dir, fmt.Sprintf("f%02d.log", i), "line")
	}
	permissions := []string{"readfiles:^" + regexp.QuoteMeta(dir) + "/"}
	tests := []struct {
		name   string
		limits probeListLimits
		max    int
	}{
		{name: "scan bound", limits: probeListLimits{maxScan: 10}, max: 10},
		{name: "check bound", limits: probeListLimits{maxChecks: 5}, max: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newProbeTestHandler(t, permissions, nil)
			handler.probeListLimits = tt.limits
			answer := probeList(t, handler, dir+"/")
			if len(answer.Entries) == 0 || len(answer.Entries) > tt.max || !answer.Truncated {
				t.Fatalf("%d entries, truncated %v, want 1 to %d and truncated", len(answer.Entries),
					answer.Truncated, tt.max)
			}
		})
	}
	handler := newProbeTestHandler(t, permissions, nil)
	if answer := probeList(t, handler, dir+"/"); len(answer.Entries) != 40 || answer.Truncated {
		t.Fatalf("default limits: %d entries, truncated %v, want 40", len(answer.Entries), answer.Truncated)
	}
}

// TestProbeListTruncatedWithoutEntries checks that reaching a bound of the
// scan or of the permission checks sets Truncated also when the listing
// offers no entry, so the client fetches a narrower prefix instead of
// taking the empty listing as complete: a permitted name that sorts after
// more than maxChecks denied ones is offered for the narrower prefix.
// (Truncated thus reveals that many names not offered share the prefix, an
// accepted oracle, see probe_list.go.)
func TestProbeListTruncatedWithoutEntries(t *testing.T) {
	dir := realTempDir(t)
	for i := range 12 {
		writeProbeTestFile(t, dir, fmt.Sprintf("denied%02d.log", i), "line")
	}
	writeProbeTestFile(t, dir, "z.log", "line")
	quoted := regexp.QuoteMeta(dir)
	permissions := []string{"readfiles:^" + quoted + "/", "readfiles:!^" + quoted + "/denied"}
	handler := newProbeTestHandler(t, permissions, nil)
	handler.probeListLimits = probeListLimits{maxChecks: 10}
	for _, prefix := range []string{dir + "/", dir + "/d", dir + "/denied"} {
		if answer := probeList(t, handler, prefix); len(answer.Entries) != 0 || !answer.Truncated ||
			len(answer.Errors) != 0 {
			t.Fatalf("listing of %s = %+v, want no entries and truncated", prefix, answer)
		}
	}
	answer := probeList(t, handler, dir+"/z")
	if got := entryNames(answer); !slices.Equal(got, []string{"z.log"}) || answer.Truncated {
		t.Fatalf("listing of %s/z = %q, truncated %v, want [z.log], complete", dir, got, answer.Truncated)
	}
	// Fewer matching names than the bound: an empty listing is complete.
	for _, prefix := range []string{dir + "/denied0", dir + "/nothing"} {
		if answer := probeList(t, handler, prefix); len(answer.Entries) != 0 || answer.Truncated {
			t.Fatalf("listing of %s = %+v, want no entries, complete", prefix, answer)
		}
	}
	// The scan bound, too, leaves an empty listing truncated.
	handler.probeListLimits = probeListLimits{maxScan: 5}
	if answer := probeList(t, handler, dir+"/denied"); len(answer.Entries) != 0 || !answer.Truncated {
		t.Fatalf("scan bound: listing = %+v, want no entries and truncated", answer)
	}
}

// TestProbeListScanTimeout checks that a scan stopped by the probe's
// timeout before any name matched answers the timeout error and Truncated,
// not an empty listing the client would take as complete.
func TestProbeListScanTimeout(t *testing.T) {
	dir := realTempDir(t)
	writeProbeTestFile(t, dir, "app.log", "line")
	handler := newProbeTestHandler(t, []string{"readfiles:^" + regexp.QuoteMeta(dir) + "/"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	answer := handler.newProber().runList(ctx, protocol.ProbeRequest{ID: "l1", List: dir + "/"})
	want := []protocol.ProbeError{{Error: protocol.ProbeErrorTimeout}}
	if len(answer.Entries) != 0 || !answer.Truncated || !slices.Equal(answer.Errors, want) {
		t.Fatalf("listing with the timeout reached = %+v, want truncated and %+v", answer, want)
	}
	// Without the timeout, the same listing offers the file.
	answer = handler.newProber().runList(context.Background(), protocol.ProbeRequest{ID: "l1", List: dir + "/"})
	if got := entryNames(answer); !slices.Equal(got, []string{"app.log"}) || answer.Truncated ||
		len(answer.Errors) != 0 {
		t.Fatalf("listing = %+v, want [app.log]", answer)
	}
}

// TestProbeListScanKeepsBoundedNames checks that the scan of a directory
// keeps no more names than the listing may check (at most twice as many
// while it reads), still the first ones in sorted order, and reports that
// it dropped some.
func TestProbeListScanKeepsBoundedNames(t *testing.T) {
	dir := realTempDir(t)
	const files = 50
	for i := range files {
		// Created in reverse order, so the directory order is unlikely to
		// be the sorted one.
		writeProbeTestFile(t, dir, fmt.Sprintf("f%02d.log", files-1-i), "line")
	}
	handler := newProbeTestHandler(t, nil, nil)
	r := &listRun{prober: handler.newProber(), answer: &protocol.ProbeAnswer{}}
	r.listLimits = probeListLimits{maxChecks: 7}.withDefaults()
	names, truncated := r.scanDir(context.Background(), dir, "f")
	want := []string{"f00.log", "f01.log", "f02.log", "f03.log", "f04.log", "f05.log", "f06.log"}
	if !slices.Equal(names, want) || !truncated {
		t.Fatalf("scan = %q, truncated %v, want %q and truncated", names, truncated, want)
	}
	r.listLimits = probeListLimits{maxChecks: files}.withDefaults()
	if names, truncated := r.scanDir(context.Background(), dir, "f"); len(names) != files || truncated {
		t.Fatalf("scan with room for every name: %d names, truncated %v", len(names), truncated)
	}
}

// TestProbeListServerless checks that the serverless runtime's handler
// lists paths the same way.
func TestProbeListServerless(t *testing.T) {
	tree := newProbeListTree(t)
	handler := newProbeTestHandler(t, tree.permissions(), &strings.Builder{})
	answer := probeList(t, handler, tree.logs+"/s")
	if got := entryNames(answer); !slices.Equal(got, []string{"sub/"}) {
		t.Fatalf("serverless listing = %q, want [sub/]", got)
	}
}

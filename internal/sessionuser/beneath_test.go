package sessionuser

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

// TestMayReadBeneath checks which directories the path listing may offer: a
// directory only when some path beneath it may be permitted, so that the
// names of directories leading to no readable file stay hidden.
func TestMayReadBeneath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		perms []string
		dir   string
		want  bool
	}{
		{name: "no rules", dir: "/var/log", want: false},
		{name: "rule of another type", perms: []string{"other:^/var/log/"}, dir: "/var/log", want: false},
		{name: "anchored allow, the directory itself", perms: []string{"readfiles:^/var/log/"},
			dir: "/var/log", want: true},
		{name: "anchored allow, a directory beneath", perms: []string{"readfiles:^/var/log/"},
			dir: "/var/log/app", want: true},
		{name: "anchored allow, an ancestor", perms: []string{"readfiles:^/var/log/app/.*\\.log$"},
			dir: "/var", want: true},
		{name: "anchored allow, the root", perms: []string{"readfiles:^/var/log/"}, dir: "/", want: true},
		{name: "anchored allow, a sibling", perms: []string{"readfiles:^/var/log/app/"},
			dir: "/var/log/secret", want: false},
		{name: "anchored allow, a sibling sharing a name prefix", perms: []string{"readfiles:^/var/log/app/"},
			dir: "/var/log/application", want: false},
		{name: "anchored allow, another tree", perms: []string{"readfiles:^/var/log/"}, dir: "/etc", want: false},
		{name: "allow of a single file, its directory", perms: []string{"readfiles:^/var/log/app\\.log$"},
			dir: "/var/log", want: true},
		{name: "allow of a single file, a directory beside it", perms: []string{"readfiles:^/var/log/app\\.log$"},
			dir: "/var/log/app", want: false},
		{name: "end-anchored path is no directory", perms: []string{"readfiles:^/var/log$"},
			dir: "/var/log", want: false},
		{name: "end anchor right after the directory", perms: []string{"readfiles:^/var/log/$"},
			dir: "/var/log", want: false},
		{name: "unanchored suffix rule", perms: []string{"readfiles:\\.log$"}, dir: "/etc/ssl", want: true},
		{name: "unanchored rule matching inside the path", perms: []string{"readfiles:/app/"},
			dir: "/srv/app/logs", want: true},
		{name: "bare rule without type", perms: []string{"^/var/log/"}, dir: "/var/log/x", want: true},
		{name: "deny covering the directory", perms: []string{"readfiles:^/var/log/", "readfiles:!^/var/log/secret/"},
			dir: "/var/log/secret", want: false},
		{name: "deny covering an ancestor", perms: []string{"readfiles:^/", "readfiles:!^/var/log/secret"},
			dir: "/var/log/secret/deeper", want: false},
		{name: "documented deny form hides its directory",
			perms: []string{"readfiles:!^/var/log/secret/.*$", "readfiles:^/var/log/.*$"},
			dir:   "/var/log/secret", want: false},
		{name: "documented deny form hides directories beneath",
			perms: []string{"readfiles:!^/var/log/secret/.*$", "readfiles:^/var/log/.*$"},
			dir:   "/var/log/secret/sub", want: false},
		{name: "documented deny form keeps the parent",
			perms: []string{"readfiles:!^/var/log/secret/.*$", "readfiles:^/var/log/.*$"},
			dir:   "/var/log", want: true},
		// A deny rule matching only at the end of the directory path hides
		// the directory only when it matches the sample paths beneath too.
		{name: "deny of the directory path alone keeps it",
			perms: []string{"readfiles:^/x/", "readfiles:!^/x/$"}, dir: "/x", want: true},
		{name: "deny of the files right in a directory keeps it",
			perms: []string{"readfiles:^/var/log/", "readfiles:!^/var/log/[^/]*$"}, dir: "/var/log", want: true},
		{name: "deny of the files right in a directory keeps one beneath",
			perms: []string{"readfiles:^/var/log/", "readfiles:!^/var/log/[^/]*$"}, dir: "/var/log/app", want: true},
		{name: "deny of an optional subtree keeps the directory",
			perms: []string{"readfiles:^/var/log/", "readfiles:!^/var/log/(old/.*)?$"}, dir: "/var/log", want: true},
		{name: "deny of an optional subtree hides the subtree",
			perms: []string{"readfiles:^/var/log/", "readfiles:!^/var/log/(old/.*)?$"},
			dir:   "/var/log/old", want: false},
		{name: "deny of the files right in the root keeps the root",
			perms: []string{"readfiles:^/", "readfiles:!^/[^/]*$"}, dir: "/", want: true},
		{name: "deny of names starting with a word character keeps the directory",
			perms: []string{"readfiles:^/data/", `readfiles:!^/data/\b`}, dir: "/data", want: true},
		{name: "deny of names without a dot keeps the directory",
			perms: []string{"readfiles:^/data/", "readfiles:!^/data/[^.]*$"}, dir: "/data", want: true},
		// The deepest sample (y/y/y/y.y) is four levels down, so a rule
		// denying fewer levels keeps the directory, whose deeper files are
		// readable; one denying four levels is still taken for the whole.
		{name: "deny of one and two levels keeps the directory",
			perms: []string{"readfiles:^/data/", "readfiles:!^/data/[^/]*(/[^/]*)?$"}, dir: "/data", want: true},
		{name: "deny of one and two levels by an optional slash keeps the directory",
			perms: []string{"readfiles:^/home/", "readfiles:!^/home/[^/]*/?[^/]*$"}, dir: "/home", want: true},
		{name: "deny of one and two levels keeps a directory beneath",
			perms: []string{"readfiles:^/home/", "readfiles:!^/home/[^/]*/?[^/]*$"}, dir: "/home/u", want: true},
		{name: "deny of the sample names only keeps the directory",
			perms: []string{"readfiles:^/data/", `readfiles:!^/data/(x|x/x|\.x)?$`}, dir: "/data", want: true},
		{name: "deny of four levels only still hides (the samples reach no deeper)",
			perms: []string{"readfiles:^/data/", "readfiles:!^/data/[^/]*(/[^/]*){0,3}$"}, dir: "/data", want: false},
		{name: "deny of everything beneath by a class still hides",
			perms: []string{"readfiles:^/data/", "readfiles:!^/data/([^/]|/)*$"}, dir: "/data", want: false},
		{name: "deny of everything beneath ending in a line end still hides",
			perms: []string{"readfiles:^/data/", "readfiles:!(?m)^/data/.*$"}, dir: "/data", want: false},
		// Known approximation (PathRules): a deny rule that denies everything
		// beneath only through what follows the "/" does not hide it.
		{name: "deny of every file beneath by suffix is not seen",
			perms: []string{`readfiles:^/var/log/.*\.log$`, `readfiles:!^/var/log/private/.*\.log$`},
			dir:   "/var/log/private", want: true},
		{name: "deny of every name beneath by a class is not seen",
			perms: []string{"readfiles:^/data/", "readfiles:!^/data/x/[^/]"}, dir: "/data/x", want: true},
		// Known approximation: a deny rule of everything beneath that does not
		// match "<dir>/" itself (".+" where the documented form has ".*").
		{name: "deny of everything beneath but not of the directory path is not seen",
			perms: []string{"readfiles:^/var/log/", "readfiles:!^/var/log/secret/.+$"},
			dir:   "/var/log/secret", want: true},
		{name: "the documented form beside it hides",
			perms: []string{"readfiles:^/var/log/", "readfiles:!^/var/log/secret/.*$"},
			dir:   "/var/log/secret", want: false},
		// Known approximation: a deny rule matching "<dir>/" at its end but
		// not the samples, while it denies all the allow rule covers.
		{name: "deny of all an allow rule covers beneath, by suffix, is not seen",
			perms: []string{`readfiles:^/var/log/.*\.log$`, `readfiles:!^/var/log/private/(.*\.log)?$`},
			dir:   "/var/log/private", want: true},
		{name: "deny of all an allow rule covers beneath, by a subdirectory, is not seen",
			perms: []string{"readfiles:^/srv/app/[^/]+/logs/", "readfiles:!^/srv/app/secret/(logs/.*)?$"},
			dir:   "/srv/app/secret", want: true},
		// Assertions about the rune after the directory path: each kind of
		// next rune on its own, so contradictory ones are not satisfied.
		{name: "contradictory assertions at the end keep no allow rule alive",
			perms: []string{`readfiles:^/var/log/\b\Bx`}, dir: "/var/log", want: false},
		{name: "contradictory assertions within the path",
			perms: []string{`readfiles:^/var/\b\Blog/`}, dir: "/var/log", want: false},
		{name: "allow rule alive only through a newline (no such path is readable)",
			perms: []string{"readfiles:(?m)^/data/$"}, dir: "/data", want: false},
		{name: "multi-line allow rule of a file", perms: []string{`readfiles:(?m)^/data/app\.log$`},
			dir: "/data", want: true},
		{name: "non-word boundary at the end", perms: []string{`readfiles:^/var/log/\B\.x`},
			dir: "/var/log", want: true},
		// Unanchored allow rules that match at the end of a longer path, or
		// behind another rune than the directory path's "/": every
		// directory may hold a file they permit.
		{name: "allow of every path by its end", perms: []string{"readfiles:$"}, dir: "/d/x", want: true},
		{name: "allow of every path by a line end", perms: []string{"readfiles:(?m)$"}, dir: "/d/x", want: true},
		{name: "allow of every path by the end of the text", perms: []string{`readfiles:\z`},
			dir: "/d/x", want: true},
		{name: "allow of paths ending in a word rune", perms: []string{`readfiles:\b$`}, dir: "/d/x", want: true},
		{name: "allow by an alternative of the end", perms: []string{`readfiles:^/d/s/(?m).+|$`},
			dir: "/d/x", want: true},
		{name: "allow of paths ending in no word rune", perms: []string{`readfiles:\B(?m)$`},
			dir: "/d/x", want: true},
		{name: "allow of a name inside a word", perms: []string{`readfiles:\Bfoo`}, dir: "/d", want: true},
		{name: "allow anchored at a line start is not unanchored",
			perms: []string{`readfiles:(?m)^/data/app\.log$`}, dir: "/etc", want: false},
		// The samples include names of no word character.
		{name: "deny of paths with a word character keeps the directory",
			perms: []string{"readfiles:^/d/", `readfiles:!^/d/(?s).+\b`}, dir: "/d/-", want: true},
		{name: "deny of paths with a word character keeps the parent",
			perms: []string{"readfiles:^/d/", `readfiles:!^/d/(?s).+\b`}, dir: "/d", want: true},
		// Known approximation: the thread's own rune is not compared with
		// the kind of next rune that satisfied its assertion.
		{name: "assertion and rune that exclude each other are not seen",
			perms: []string{`readfiles:^/d/\b-`}, dir: "/d", want: true},
		{name: "deny before the allow still wins", perms: []string{"readfiles:!^/var/log/secret/", "readfiles:^/var/log/"},
			dir: "/var/log/secret", want: false},
		{name: "deny of some files only", perms: []string{"readfiles:^/var/log/", "readfiles:!\\.gz$"},
			dir: "/var/log/app", want: true},
		{name: "deny of a sibling", perms: []string{"readfiles:^/var/log/", "readfiles:!^/var/log/secret/"},
			dir: "/var/log/app", want: true},
		{name: "invalid regex fails closed", perms: []string{"readfiles:^/var/log/(", "readfiles:^/"},
			dir: "/var/log", want: false},
		{name: "journal rule offers no directory", perms: []string{"readfiles:^journal:"}, dir: "/var/log", want: false},
		{name: "word boundary at the end", perms: []string{"readfiles:^/var/log/\\bapp"}, dir: "/var/log", want: true},
		{name: "multi-byte path", perms: []string{"readfiles:^/var/log/ü/"}, dir: "/var/log/ü", want: true},
		{name: "multi-byte sibling", perms: []string{"readfiles:^/var/log/ü/"}, dir: "/var/log/ä", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := newTestUser(tt.perms).PathRules("readfiles").MayReadBeneath(tt.dir); got != tt.want {
				t.Fatalf("MayReadBeneath(%q) with %q = %v, want %v", tt.dir, tt.perms, got, tt.want)
			}
		})
	}
}

// TestMayReadBeneathUncheckedUsers checks that dserver's own jobs, which the
// read permissions do not apply to, may list every directory.
func TestMayReadBeneathUncheckedUsers(t *testing.T) {
	t.Parallel()
	for _, name := range []string{config.ScheduleUser, config.ContinuousUser} {
		u := newTestUser(nil)
		u.Name = name
		if !u.PathRules("readfiles").MayReadBeneath("/etc") {
			t.Fatalf("MayReadBeneath for %s = false, want true", name)
		}
	}
}

// TestMayReadBeneathAgreesWithFileChecks checks MayReadBeneath against the
// file permission check: every directory above a permitted file must be
// offered (or the file could not be reached by descending), and no directory
// may be offered whose files are all denied.
func TestMayReadBeneathAgreesWithFileChecks(t *testing.T) {
	t.Parallel()

	rules := [][]string{
		{"readfiles:^/var/log/app/.*\\.log$"},
		{"readfiles:^/var/log/", "readfiles:!^/var/log/secret/"},
		{"readfiles:\\.log$", "readfiles:!^/root/"},
		{"readfiles:^/srv/(a|bb)/[0-9]+/x$"},
		// Deny rules that match a directory path only at its end ("$") but
		// not everything beneath it: the directory holds readable files.
		{"readfiles:^/var/log/", "readfiles:!^/var/log/[^/]*$"},
		{"readfiles:^/var/log/", "readfiles:!^/var/log/(old/.*)?$"},
		{"readfiles:^/", "readfiles:!^/[^/]*$"},
		{"readfiles:^/var/log/", `readfiles:!/\.[^/]*$`},
		// Deny rules of some levels only, of the former sample names only,
		// and of everything but one deep name: the deeper files are readable.
		{"readfiles:^/", "readfiles:!^/home/[^/]*/?[^/]*$"},
		{"readfiles:^/", "readfiles:!^/var/log/[^/]*(/[^/]*)?$"},
		{"readfiles:^/", `readfiles:!^/var/log/app/(x|x/x|\.x)?$`},
		{"readfiles:^/", `readfiles:!^/home/u/([^y].*|y|y/[^y].*)?$`},
		{"readfiles:^/", `readfiles:!^/var/log/secret/.+$`},
		{`readfiles:^/var/log/.*\.log$`, `readfiles:!^/var/log/private/(.*\.log)?$`},
		{`readfiles:^/var/\b\Blog/`, `readfiles:^/srv/\ba`, `readfiles:(?m)^/etc/passwd$`},
		// Unanchored allow rules matching at the end of the path only.
		{"readfiles:$"},
		{"readfiles:(?m)$"},
		{`readfiles:\b$`, "readfiles:!^/root/"},
		{`readfiles:^/d/s/(?m).+|$`},
		{`readfiles:\B(?m)$`},
		{`readfiles:\Bog/`},
		// A deny rule of the paths with a word character only.
		{"readfiles:^/", `readfiles:!^/var/log/(?s).+\b`},
	}
	files := []string{
		"/var/log/app/a.log", "/var/log/app/sub/b.log", "/var/log/app/a.txt", "/var/log/secret/s.log",
		"/var/log/other.log", "/root/r.log", "/etc/passwd", "/srv/a/12/x", "/srv/bb/3/x", "/srv/c/1/x",
		"/home/u", "/home/u/f", "/home/u/d/f", "/home/u/y/y/y/y.y", "/home/u/y/z", "/home/u/a/b/c/d/e.log",
		"/var/log/app/x", "/var/log/app/x/x", "/var/log/app/.x", "/var/log/app/a/b/c/d.log",
		"/var/log/private/a.log", "/var/log/private/a.txt", "/var/log/secret/a/b/c/d/e",
		"/var/log/-", "/var/log/-/-", "/var/log/app/-/--", "/srv/a/-", "/d/x/y-",
	}
	for _, perms := range rules {
		u := newTestUser(perms)
		for _, file := range files {
			permitted, err := u.iteratePaths(file, "readfiles", u.checkLog(false))
			if err != nil {
				t.Fatal(err)
			}
			for dir := filepath.Dir(file); ; dir = filepath.Dir(dir) {
				if permitted && !u.PathRules("readfiles").MayReadBeneath(dir) {
					t.Errorf("rules %q: %s is permitted but its directory %s is not offered", perms, file, dir)
				}
				if dir == "/" {
					break
				}
			}
		}
	}
	denied := newTestUser(rules[1])
	if denied.PathRules("readfiles").MayReadBeneath("/var/log/secret") {
		t.Errorf("rules %q: /var/log/secret is offered although every file beneath is denied", rules[1])
	}
}

// TestPathRulesMayAllowPrefix checks the gate of the journal unit listing:
// only allow rules that can match a target with the prefix count.
func TestPathRulesMayAllowPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		perms  []string
		prefix string
		want   bool
	}{
		{perms: []string{`readfiles:^journal:app-.*\.service$`}, prefix: "journal:", want: true},
		{perms: []string{`readfiles:^journal:app-.*\.service$`}, prefix: "journal:app-w", want: true},
		{perms: []string{`readfiles:^journal:app-.*\.service$`}, prefix: "journal:ssh", want: false},
		{perms: []string{"readfiles:^/var/log/"}, prefix: "journal:", want: false},
		{perms: []string{`readfiles:\.service$`}, prefix: "journal:", want: true},
		{perms: []string{"readfiles:!^journal:"}, prefix: "journal:", want: false},
		{perms: []string{"readfiles:^journal:(", "readfiles:^journal:"}, prefix: "journal:", want: false},
		// The target may be the prefix itself: a unit name typed in full.
		{perms: []string{`readfiles:^journal:app\.service$`}, prefix: "journal:app.service", want: true},
		{perms: []string{`readfiles:^journal:app\.service$`}, prefix: "journal:app.servicex", want: false},
		{perms: []string{`readfiles:^journal:app\.service$`, `readfiles:!^journal:app`},
			prefix: "journal:app.service", want: true},
		{perms: []string{`readfiles:^journal:\b\Bapp`}, prefix: "journal:", want: false},
		// Unanchored rules matching at the end of a longer target.
		{perms: []string{`readfiles:\b$`}, prefix: "journal:", want: true},
		{perms: []string{"readfiles:$"}, prefix: "journal:", want: true},
		{perms: []string{`readfiles:!\b$`}, prefix: "journal:", want: false},
		{prefix: "journal:", want: false},
	}
	for _, tt := range tests {
		if got := newTestUser(tt.perms).PathRules("readfiles").MayAllowPrefix(tt.prefix); got != tt.want {
			t.Errorf("MayAllowPrefix(%q) with %q = %v, want %v", tt.prefix, tt.perms, got, tt.want)
		}
	}
	scheduled := newTestUser(nil)
	scheduled.Name = config.ScheduleUser
	if !scheduled.PathRules("readfiles").MayAllowPrefix("journal:") {
		t.Error("MayAllowPrefix for the scheduler = false, want true")
	}
}

// TestPathRulesReuse checks that one PathRules answers many questions in a
// row the same as fresh ones (the reused closure marks).
func TestPathRulesReuse(t *testing.T) {
	t.Parallel()
	perms := []string{"readfiles:^/var/log/app/", "readfiles:!^/var/log/app/secret/"}
	rules := newTestUser(perms).PathRules("readfiles")
	dirs := []string{"/var/log/app", "/var/log/app/secret", "/etc", "/var", "/var/log/app/x/y"}
	for range 3 {
		for _, dir := range dirs {
			if got, want := rules.MayReadBeneath(dir), newTestUser(perms).PathRules("readfiles").MayReadBeneath(dir); got != want {
				t.Fatalf("reused rules: MayReadBeneath(%q) = %v, fresh = %v", dir, got, want)
			}
		}
	}
}

// TestPathRulesWarnsInvalidRulesOncePerUserAndType checks that the rules
// that do not compile are logged at WARN by the user's first listing of a
// permission type only (any user starts a listing with Tab), while every
// listing still offers nothing, that the rules of another type are not
// logged with it, and that they are logged by the first listing of their
// own type: one type's warning does not silence another's.
func TestPathRulesWarnsInvalidRulesOncePerUserAndType(t *testing.T) {
	t.Parallel()
	logger := &levelLogger{}
	u := newTestUser([]string{"readfiles:^/var/log/(", "readfiles:^/", "readfiles:!^/etc/[", "other:^/x/("})
	u.logger = logger
	warnings := func() int {
		count := 0
		for _, line := range logger.take() {
			if strings.HasPrefix(line, "WARN") {
				count++
			}
		}
		return count
	}
	for listing := range 3 {
		rules := u.PathRules("readfiles")
		if rules.MayReadBeneath("/var/log") || rules.MayAllowPrefix("journal:") {
			t.Fatalf("listing %d: rules with an invalid rule offer something", listing)
		}
		want := 0
		if listing == 0 {
			want = 2
		}
		if got := warnings(); got != want {
			t.Fatalf("listing %d: %d warnings, want %d", listing, got, want)
		}
	}
	for listing, want := range []int{1, 0} {
		if u.PathRules("other").MayReadBeneath("/x") {
			t.Fatalf("listing %d of the other type offers something", listing)
		}
		if got := warnings(); got != want {
			t.Fatalf("listing %d of the other type: %d warnings, want %d", listing, got, want)
		}
	}
	if u.PathRules("valid").invalid || warnings() != 0 {
		t.Fatal("a type without invalid rules is invalid or warned about")
	}
}

// TestPathRulesOfOneUserConcurrently checks that listings of one user may
// run at the same time: each PathRules has the scratch state of its runs to
// itself, only the compiled rules are shared (run with -race).
func TestPathRulesOfOneUserConcurrently(t *testing.T) {
	t.Parallel()
	u := newTestUser([]string{"readfiles:^/var/log/app/", "readfiles:!^/var/log/app/secret/.*$"})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rules := u.PathRules("readfiles")
			for range 200 {
				if !rules.MayReadBeneath("/var/log/app") || rules.MayReadBeneath("/var/log/app/secret") {
					t.Error("unexpected answer of concurrently used rules")
					return
				}
			}
		}()
	}
	wg.Wait()
}

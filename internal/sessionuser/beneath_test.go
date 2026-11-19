package sessionuser

import (
	"path/filepath"
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
		{name: "deny of the directory path alone hides it (errs towards hiding)",
			perms: []string{"readfiles:^/x/", "readfiles:!^/x/$"}, dir: "/x", want: false},
		// Known approximation (PathRules): a deny rule that denies everything
		// beneath only through what follows the "/" does not hide it.
		{name: "deny of every file beneath by suffix is not seen",
			perms: []string{`readfiles:^/var/log/.*\.log$`, `readfiles:!^/var/log/private/.*\.log$`},
			dir:   "/var/log/private", want: true},
		{name: "deny of every name beneath by a class is not seen",
			perms: []string{"readfiles:^/data/", "readfiles:!^/data/x/[^/]"}, dir: "/data/x", want: true},
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
			if got := newTestUser(tt.perms).MayReadBeneath(tt.dir, "readfiles"); got != tt.want {
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
		if !u.MayReadBeneath("/etc", "readfiles") {
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
	}
	files := []string{
		"/var/log/app/a.log", "/var/log/app/sub/b.log", "/var/log/app/a.txt", "/var/log/secret/s.log",
		"/var/log/other.log", "/root/r.log", "/etc/passwd", "/srv/a/12/x", "/srv/bb/3/x", "/srv/c/1/x",
	}
	for _, perms := range rules {
		u := newTestUser(perms)
		for _, file := range files {
			permitted, err := u.iteratePaths(file, "readfiles", u.checkLog(false))
			if err != nil {
				t.Fatal(err)
			}
			for dir := filepath.Dir(file); ; dir = filepath.Dir(dir) {
				if permitted && !u.MayReadBeneath(dir, "readfiles") {
					t.Errorf("rules %q: %s is permitted but its directory %s is not offered", perms, file, dir)
				}
				if dir == "/" {
					break
				}
			}
		}
	}
	denied := newTestUser(rules[1])
	if denied.MayReadBeneath("/var/log/secret", "readfiles") {
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
			if got, want := rules.MayReadBeneath(dir), newTestUser(perms).MayReadBeneath(dir, "readfiles"); got != want {
				t.Fatalf("reused rules: MayReadBeneath(%q) = %v, fresh = %v", dir, got, want)
			}
		}
	}
}

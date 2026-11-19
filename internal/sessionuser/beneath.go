package sessionuser

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode/utf8"

	"github.com/mimecast/dtail/internal/io/fs/permissions"
)

// PathRules is the compiled form of a user's read rules of one permission
// type, for the path listing of the probe command
// (internal/handlers/probe_list.go): it tells which directories to offer
// (MayReadBeneath) and whether a journal unit listing can offer anything
// (MayAllowPrefix). A listing builds it once (User.PathRules, from the
// rules compiled once per User) and asks it about every entry. It is not
// safe for concurrent use, as its rules hold the scratch state of a run.
//
// Directory rule: a directory is offered unless no allow rule can match a
// path beneath it, or one deny rule already matches the directory path
// itself with its trailing "/". A deny rule that matches within that path
// matches every path beneath it. One that matches only at its end, through
// an assertion about what follows ("$", "\b"), hides the directory only if
// it also matches the sample paths beneath it (denySamples): so
// "!^/var/log/secret/.*$" hides /var/log/secret, while "!^/var/log/[^/]*$"
// (the files right in /var/log), "!^/var/log/(old/.*)?$" and
// "!^/home/[^/]*/?[^/]*$" (one and two levels) leave their directory
// offered, whose subdirectories hold readable files. A deny rule that
// matches "<dir>/" itself and everything beneath it ("!^<dir>/",
// "!^<dir>/.*$") therefore always hides the directory, as every sample is
// a path beneath it; one that denies the samples but not every path (e.g.
// only up to four levels) still hides it.
//
// The rule is an approximation, towards offering. A directory's name is
// offered although nothing beneath it is readable when
//   - a deny rule denies everything beneath it without matching "<dir>/"
//     itself: "!^/var/log/secret/.+$" (write ".*$", or "!^/var/log/secret/"),
//     or through what follows the "/" ("!^/var/log/private/.*\.log$" next
//     to the allow rule "^/var/log/.*\.log$", "!^/data/x/[^/]" next to
//     "^/data/");
//   - a deny rule matches "<dir>/" at its end but not the samples, while
//     denying all the allow rules cover ("!^/var/log/private/(.*\.log)?$"
//     next to "^/var/log/.*\.log$");
//   - several deny rules deny everything beneath it only together.
//
// The name is one an allow rule covers, and a glob read's "no permission"
// versus "no file" answers reveal as much. A file is never offered
// without the read check itself.
type PathRules struct {
	user  *User
	rules []pathRule
	// invalid is set when a rule of the type does not compile; then, like
	// iteratePaths for every file, nothing is offered.
	invalid bool
}

// pathRule is one compiled allow or deny rule, with the scratch state of
// its runs over a prefix.
type pathRule struct {
	deny bool
	prog *syntax.Prog
	// re is the rule as the file permission check matches it; a deny rule
	// matches the sample paths with it (deniesSamples).
	re *regexp.Regexp
	// marks and generation make the closure's visited set reusable: an
	// instruction was visited in the current closure when its mark equals
	// generation.
	marks      []uint32
	generation uint32
	// later caches matchesLater once laterKnown is set.
	later, laterKnown bool
}

// prefixMatch tells where a rule matched a prefix (pathRule.prefixState).
type prefixMatch int

const (
	// noMatch: the rule did not match within the prefix.
	noMatch prefixMatch = iota
	// matchWithin: the rule matched before the end of the prefix, so it
	// matches every path starting with the prefix.
	matchWithin
	// matchAtEnd: the rule matched only at the end of the prefix, under
	// assertions about the rune after it, which is unknown: it matches
	// some paths starting with the prefix, or the prefix alone.
	matchAtEnd
)

// denySamples are the paths beneath a directory, relative to it, a deny
// rule that matched only at the end of the directory path (matchAtEnd) must
// match as well to hide the directory: a name, a path one level deeper, a
// name starting with no word character and holding a ".", a path four
// levels deep of other letters with a "." in its last name, and a name and
// a path of no word character at all, for rules such as "[^/]*$",
// "(old/.*)?$", "\b", "[^.]*$", "[^/]*/?[^/]*$" (one and two levels only)
// and "(?s).+\b" (paths with a word character), which deny only some of
// the paths beneath. They can only make a directory offered that the rule
// does not deny as a whole.
var denySamples = [...]string{"x", "x/x", ".x", "y/y/y/y.y", "-", "-/-"}

// PathRules returns the user's rules of permissionType for one listing.
// The rules themselves are compiled once per User (compiledPermissions);
// only the scratch state of their runs is new. A rule that does not
// compile is logged at WARN once per User and permission type (the first
// listing of the type), not per listing, which any user starts with Tab.
func (u *User) PathRules(permissionType string) *PathRules {
	rules := &PathRules{user: u}
	var invalid []error
	for _, permission := range u.compiledPermissions() {
		if permission.typeStr != permissionType {
			continue
		}
		if permission.err != nil {
			invalid = append(invalid, permission.err)
			continue
		}
		rules.rules = append(rules.rules, pathRule{
			deny:  permission.deny,
			prog:  permission.prog,
			re:    permission.re,
			marks: make([]uint32, len(permission.prog.Inst)),
		})
	}
	if len(invalid) > 0 {
		rules.invalid = true
		if u.firstInvalidRulesWarning(permissionType) {
			for _, err := range invalid {
				u.log().Warn(u, err)
			}
		}
	}
	return rules
}

// MayReadBeneath reports whether to offer the directory dir (an absolute
// path with symlinks already resolved), by the directory rule of
// PathRules. Like ResolveReadTarget, it first asks the OS permission check
// (permissions.ToRead, a Linux ACL check in linuxacl builds) about dir, and
// offers everything to dserver's own scheduled and continuous jobs.
func (r *PathRules) MayReadBeneath(dir string) bool {
	u := r.user
	if !u.permissionChecked() {
		return true
	}
	if r.invalid || strings.ContainsRune(dir, '\n') {
		// No path with a newline is readable (iteratePaths), so nothing
		// beneath such a directory is.
		return false
	}
	if _, err := permissions.ToRead(u.Name, dir, u.log()); err != nil {
		u.log().Debug(u, dir, "No OS file system permissions to read directory", err)
		return false
	}
	prefix := dir
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	allowed := false
	for i := range r.rules {
		rule := &r.rules[i]
		if !rule.deny {
			allowed = allowed || rule.mayMatchFrom(prefix, false)
			continue
		}
		if match, _ := rule.prefixState(prefix, true); rule.deniesBeneath(prefix, match) {
			return false
		}
	}
	return allowed
}

// deniesBeneath reports whether the deny rule, which matched prefix (a
// directory path with its trailing "/") as match says, hides the directory:
// it matched within the prefix, or at its end and matches every sample path
// beneath it too (denySamples).
func (rule *pathRule) deniesBeneath(prefix string, match prefixMatch) bool {
	switch match {
	case matchWithin:
		return true
	case matchAtEnd:
		for _, sample := range denySamples {
			if !rule.re.MatchString(prefix + sample) {
				return false
			}
		}
		return true
	}
	return false
}

// MayAllowPrefix reports whether an allow rule may match some path (or
// journal target) starting with prefix, deny rules not considered. The
// journal unit listing asks it before it runs journalctl, so a user whose
// rules can match no "journal:" target spawns none. The target may be the
// prefix itself.
func (r *PathRules) MayAllowPrefix(prefix string) bool {
	if !r.user.permissionChecked() {
		return true
	}
	if r.invalid {
		return false
	}
	for i := range r.rules {
		rule := &r.rules[i]
		if rule.deny {
			continue
		}
		// The target may be the prefix itself (a unit name typed in full),
		// so a rule ending in "$" may match at its end.
		if rule.mayMatchFrom(prefix, true) {
			return true
		}
	}
	return false
}

// compilePermission compiles the regex of a permission rule: into the
// regexp the file permission check matches with, and, the way
// regexp.Compile does it (Perl syntax), into the program prefixState runs.
func compilePermission(rule string) (*regexp.Regexp, *syntax.Prog, error) {
	re, err := regexp.Compile(rule)
	if err != nil {
		return nil, nil, fmt.Errorf("permission test failed, can't compile regex '%s': %w", rule, err)
	}
	parsed, err := syntax.Parse(rule, syntax.Perl)
	if err != nil {
		return nil, nil, fmt.Errorf("permission test failed, can't compile regex '%s': %w", rule, err)
	}
	prog, err := syntax.Compile(parsed.Simplify())
	if err != nil {
		return nil, nil, fmt.Errorf("permission test failed, can't compile regex '%s': %w", rule, err)
	}
	return re, prog, nil
}

// prefixState runs the rule, unanchored like regexp.MatchString, over
// prefix and reports where it matched (prefixMatch) and whether some path
// starting with prefix may match (alive; always true when it matched).
// endOfText tells whether the text may end with prefix, so that "$"
// (syntax.EmptyEndText) can be satisfied there and a match is a
// matchAtEnd: deny rules use it, and confirm such a match with sample
// paths (deniesBeneath); so does MayAllowPrefix, whose journal target may
// be the prefix itself.
//
// It is a Pike VM over the instructions of regexp/syntax: a thread set of
// rune-consuming instructions, advanced rune by rune, with a new thread at
// the program start at every position (the unanchored start). Zero-width
// assertions are evaluated with the runes around each position; at the end
// of prefix the next rune is unknown (endOfPrefix).
func (rule *pathRule) prefixState(prefix string, endOfText bool) (match prefixMatch, alive bool) {
	var pending []uint32
	prev := rune(-1)
	for i := 0; ; {
		r, size := utf8.DecodeRuneInString(prefix[i:])
		if size == 0 {
			return rule.endOfPrefix(pending, prev, endOfText)
		}
		threads, matched := rule.closure(append(pending, uint32(rule.prog.Start)),
			syntax.EmptyOpContext(prev, r))
		if matched {
			return matchWithin, true
		}
		pending = pending[:0]
		for _, pc := range threads {
			if inst := &rule.prog.Inst[pc]; inst.MatchRune(r) {
				pending = append(pending, inst.Out)
			}
		}
		prev = r
		i += size
	}
}

// mayMatchFrom reports whether the allow rule may match some path starting
// with prefix (or, with endOfText, the prefix itself): it is alive at the
// end of the prefix (prefixState), or it may start a match of its own
// somewhere after it (matchesLater).
func (rule *pathRule) mayMatchFrom(prefix string, endOfText bool) bool {
	if _, alive := rule.prefixState(prefix, endOfText); alive {
		return true
	}
	return rule.matchesLater()
}

// matchesLater reports whether the rule, being unanchored, may start a
// match at a position after any prefix: behind a rune of the path that
// follows the prefix, and so with runes around it the prefix says nothing
// about. prefixState starts a thread at the end of the prefix too, but
// only with the prefix's last rune before it and a rune after it, which
// misses rules that match at the end of a longer path ("$", `\z`, `\b$`,
// "(?m)$", "x|$": every file is readable by them) or need another rune
// before them (`\Bfoo` behind the "/" of a directory path). The kinds of
// rune before and after such a position (a word rune, another rune, and
// the end of the text after it) are tried each on their own, as in
// endOfPrefix. The answer does not depend on the prefix, so it is
// computed once per pathRule.
//
// It only ever makes a directory offered; files are listed through the
// read check alone.
func (rule *pathRule) matchesLater() bool {
	if rule.laterKnown {
		return rule.later
	}
	rule.laterKnown = true
	start := []uint32{uint32(rule.prog.Start)}
	for _, prev := range [...]rune{'a', '/'} {
		for _, next := range endOfPrefixContexts {
			threads, matched := rule.closure(start, syntax.EmptyOpContext(prev, next))
			if matched || len(threads) > 0 {
				rule.later = true
				return true
			}
		}
	}
	return false
}

// endOfPrefix finishes prefixState at the end of the prefix, where the
// threads pending came to after its last rune prev. The next rune is
// unknown, so the zero-width assertions are tried once per kind of next
// rune (endOfPrefixContexts), each kind on its own: a rule matched, or is
// alive, when it is with one of them. Assertions no single next rune
// satisfies together (`\b\B`) are therefore not satisfied; taking the
// union of all kinds' assertions at once counted such an allow rule as
// alive and offered directories it can match nothing beneath.
//
// A remaining approximation, towards offering: a thread counts as alive
// with the kind of next rune that satisfied its assertions, although its
// own instruction may consume no rune of that kind (`^/d/\b-`: only a
// word rune makes the boundary, and "-" is none), so /d is offered.
func (rule *pathRule) endOfPrefix(pending []uint32, prev rune,
	endOfText bool) (match prefixMatch, alive bool) {

	pending = append(pending, uint32(rule.prog.Start))
	for _, next := range endOfPrefixContexts {
		if next == endOfTextRune && !endOfText {
			continue
		}
		threads, matched := rule.closure(pending, syntax.EmptyOpContext(prev, next))
		if matched {
			return matchAtEnd, true
		}
		alive = alive || len(threads) > 0
	}
	return noMatch, alive
}

// endOfTextRune is the next rune syntax.EmptyOpContext takes for the end
// of the text.
const endOfTextRune rune = -1

// endOfPrefixContexts are the kinds of rune that may follow a prefix, as
// far as zero-width assertions tell them apart: a word rune, a rune that is
// none, and the end of the text. A newline is not among them, although it
// alone satisfies "(?m)$" before the end: no path holding one is readable
// (iteratePaths), so a rule alive only through one matches nothing
// readable. The end of the text satisfies "(?m)$" as well: for the deny
// rules here, and for an allow rule at the end of a longer path
// (matchesLater).
var endOfPrefixContexts = [...]rune{'a', '/', endOfTextRune}

// closure follows the instructions that consume no rune from pcs, under the
// zero-width assertions flags, and returns the rune-consuming instructions
// reached (each once) and whether a match instruction was reached.
func (rule *pathRule) closure(pcs []uint32, flags syntax.EmptyOp) ([]uint32, bool) {
	rule.generation++
	if rule.generation == 0 { // wrapped: no stale mark may equal it
		clear(rule.marks)
		rule.generation = 1
	}
	var threads []uint32
	matched := false
	stack := append([]uint32(nil), pcs...)
	for len(stack) > 0 {
		pc := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if rule.marks[pc] == rule.generation {
			continue
		}
		rule.marks[pc] = rule.generation
		inst := &rule.prog.Inst[pc]
		switch inst.Op {
		case syntax.InstAlt, syntax.InstAltMatch:
			stack = append(stack, inst.Out, inst.Arg)
		case syntax.InstCapture, syntax.InstNop:
			stack = append(stack, inst.Out)
		case syntax.InstEmptyWidth:
			if syntax.EmptyOp(inst.Arg)&^flags == 0 {
				stack = append(stack, inst.Out)
			}
		case syntax.InstMatch:
			matched = true
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
			threads = append(threads, pc)
		}
	}
	return threads, matched
}

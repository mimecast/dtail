package sessionuser

import (
	"fmt"
	"regexp/syntax"
	"strings"
	"unicode/utf8"

	"github.com/mimecast/dtail/internal/io/fs/permissions"
)

// PathRules is the compiled form of a user's read rules of one permission
// type, for the path listing of the probe command
// (internal/handlers/probe_list.go): it tells which directories to offer
// (MayReadBeneath) and whether a journal unit listing can offer anything
// (MayAllowPrefix). A listing builds it once (User.PathRules) and asks it
// about every entry. It is not safe for concurrent use.
//
// Directory rule: a directory is offered unless no allow rule can match a
// path beneath it, or one deny rule already matches the directory path
// itself with its trailing "/" (where "$" counts as matched, so
// "!^/var/log/secret/.*$" hides /var/log/secret). This is an
// approximation: a deny rule that denies everything beneath a directory
// only through what follows the "/" (e.g. "!^/var/log/private/.*\.log$"
// next to the allow rule "^/var/log/.*\.log$", or "!^/data/x/[^/]" next to
// "^/data/"), or several deny rules that do so only together, do not hide
// it. Such a directory's name is offered although nothing beneath it is
// readable; the name is one an allow rule covers, and a glob read's
// "no permission" versus "no file" answers reveal as much.
type PathRules struct {
	user           *User
	permissionType string
	rules          []pathRule
	// invalid is set when a rule of the type does not compile; then, like
	// iteratePaths for every file, nothing is offered.
	invalid bool
}

// pathRule is one compiled allow or deny rule.
type pathRule struct {
	deny bool
	prog *syntax.Prog
	// marks and generation make the closure's visited set reusable: an
	// instruction was visited in the current closure when its mark equals
	// generation.
	marks      []uint32
	generation uint32
}

// PathRules compiles the user's rules of permissionType.
func (u *User) PathRules(permissionType string) *PathRules {
	rules := &PathRules{user: u, permissionType: permissionType}
	for _, permission := range u.permissions {
		typeStr, rule := splitPermissionType(permission)
		if typeStr != permissionType {
			continue
		}
		deny := strings.HasPrefix(rule, "!")
		prog, err := compilePermission(strings.TrimPrefix(rule, "!"))
		if err != nil {
			u.log().Warn(u, err)
			rules.invalid = true
			continue
		}
		rules.rules = append(rules.rules, pathRule{deny: deny, prog: prog, marks: make([]uint32, len(prog.Inst))})
	}
	return rules
}

// MayReadBeneath reports whether the user's read permissions of
// permissionType may permit some path beneath the directory dir (an
// absolute path with symlinks already resolved); see PathRules.
func (u *User) MayReadBeneath(dir, permissionType string) bool {
	return u.PathRules(permissionType).MayReadBeneath(dir)
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
	if r.invalid {
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
		matched, alive := rule.prefixState(prefix, rule.deny)
		switch {
		case rule.deny && matched:
			return false
		case !rule.deny && alive:
			allowed = true
		}
	}
	return allowed
}

// MayAllowPrefix reports whether an allow rule may match some path (or
// journal target) starting with prefix, deny rules not considered. The
// journal unit listing asks it before it runs journalctl, so a user whose
// rules can match no "journal:" target spawns none.
func (r *PathRules) MayAllowPrefix(prefix string) bool {
	if !r.user.permissionChecked() {
		return true
	}
	if r.invalid {
		return false
	}
	for i := range r.rules {
		rule := &r.rules[i]
		if _, alive := rule.prefixState(prefix, false); !rule.deny && alive {
			return true
		}
	}
	return false
}

// compilePermission compiles the regex of a permission rule the way
// regexp.Compile does (Perl syntax), into the program prefixState runs.
func compilePermission(rule string) (*syntax.Prog, error) {
	re, err := syntax.Parse(rule, syntax.Perl)
	if err != nil {
		return nil, fmt.Errorf("permission test failed, can't compile regex '%s': %w", rule, err)
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		return nil, fmt.Errorf("permission test failed, can't compile regex '%s': %w", rule, err)
	}
	return prog, nil
}

// prefixState runs the rule, unanchored like regexp.MatchString, over
// prefix and reports whether it matched within prefix and whether some
// path starting with prefix may match (alive; always true when matched).
// endOfText makes "$" (syntax.EmptyEndText) count as satisfied at the end
// of prefix, so a match there counts as matched: deny rules use it, which
// errs towards hiding.
//
// It is a Pike VM over the instructions of regexp/syntax: a thread set of
// rune-consuming instructions, advanced rune by rune, with a new thread at
// the program start at every position (the unanchored start). Zero-width
// assertions are evaluated with the runes around each position; at the end
// of prefix the next rune is unknown, so every assertion that some next
// rune would satisfy counts as satisfied.
func (rule *pathRule) prefixState(prefix string, endOfText bool) (matched, alive bool) {
	var pending []uint32
	prev := rune(-1)
	for i := 0; ; {
		r, size := utf8.DecodeRuneInString(prefix[i:])
		flags := endOfPrefixFlags(prev, endOfText)
		if size > 0 {
			flags = syntax.EmptyOpContext(prev, r)
		}
		threads, matched := rule.closure(append(pending, uint32(rule.prog.Start)), flags)
		if matched {
			return true, true
		}
		if size == 0 {
			return false, len(threads) > 0
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

// endOfPrefixFlags returns the zero-width assertions some next rune after
// prev may satisfy: those of a word rune, of a non-word rune and of a
// newline together, and the end of the text only when endOfText is set.
func endOfPrefixFlags(prev rune, endOfText bool) syntax.EmptyOp {
	flags := syntax.EmptyOpContext(prev, 'a') | syntax.EmptyOpContext(prev, '/') |
		syntax.EmptyOpContext(prev, '\n')
	if endOfText {
		flags |= syntax.EmptyOpContext(prev, -1)
	}
	return flags
}

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

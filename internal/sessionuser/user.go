package sessionuser

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"strings"
	"sync"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/fs/permissions"
	"github.com/mimecast/dtail/internal/logging"
)

// User represents an end-user which connected to the server via the DTail client.
type User struct {
	// The user name.
	Name string
	// The remote address connected from.
	remoteAddress string
	// The permissions the user has.
	permissions []string
	logger      logging.Logger

	// compileOnce compiles the permissions into compiled the first time a
	// check needs them (compiledPermissions), so that a check compiles no
	// regex: a probe checks up to thousands of paths per round. The
	// permissions of a User never change.
	compileOnce sync.Once
	compiled    []compiledPermission
	// invalidRulesWarned holds the permission types whose rules that do
	// not compile PathRules logged already: once per User and type, so
	// that the invalid rules of one type do not silence those of another.
	invalidRulesMu     sync.Mutex
	invalidRulesWarned map[string]struct{}
}

// firstInvalidRulesWarning reports whether the invalid rules of
// permissionType were not logged for u yet, and records that they are now.
func (u *User) firstInvalidRulesWarning(permissionType string) bool {
	u.invalidRulesMu.Lock()
	defer u.invalidRulesMu.Unlock()
	if _, warned := u.invalidRulesWarned[permissionType]; warned {
		return false
	}
	if u.invalidRulesWarned == nil {
		u.invalidRulesWarned = make(map[string]struct{})
	}
	u.invalidRulesWarned[permissionType] = struct{}{}
	return true
}

// compiledPermission is one permission rule "[<type>:][!]<regex>" in the
// forms iteratePaths (re) and the path listing's PathRules (re and prog)
// match paths with. It is immutable once compiled.
type compiledPermission struct {
	typeStr string
	// rule is the rule without its type, as logged ("!" kept).
	rule string
	deny bool
	re   *regexp.Regexp
	// prog is the same regex as the program PathRules runs over a path
	// prefix (pathRule.prefixState).
	prog *syntax.Prog
	// err is set when the regex does not compile; iteratePaths fails the
	// check when it reaches the rule, as it did compiling it per check.
	err error
}

// PermissionLookup resolves permissions for a given SSH user.
type PermissionLookup func(string) ([]string, error)

// New returns a new user.
func New(name, remoteAddress string, permissionLookup PermissionLookup,
	logger logging.Logger) (*User, error) {
	var (
		permissions []string
		err         error
	)
	if permissionLookup != nil {
		permissions, err = permissionLookup(name)
		if err != nil {
			return nil, err
		}
	}
	return &User{
		Name:          name,
		remoteAddress: remoteAddress,
		permissions:   permissions,
		logger:        logging.OrNop(logger),
	}, nil
}

// String representation of the user.
func (u *User) String() string {
	return fmt.Sprintf("%s@%s", u.Name, u.remoteAddress)
}

func (u *User) log() logging.Logger {
	return logging.OrNop(u.logger)
}

// HasFilePermission is used to determine whether user is allowed to read a file.
func (u *User) HasFilePermission(filePath, permissionType string) bool {
	_, hasPermission := u.ValidateReadTarget(filePath, permissionType)
	return hasPermission
}

// ErrReadPermissionDenied is returned by ResolveReadTarget for a path the
// user's read permissions deny.
var ErrReadPermissionDenied = errors.New("no permission to read file")

// errNewlineInPath is why iteratePaths permits no path holding a newline.
var errNewlineInPath = errors.New("permission test failed, the path holds a newline")

// ValidateReadTarget resolves and authorizes a file path for server-side reads.
func (u *User) ValidateReadTarget(filePath, permissionType string) (fs.ValidatedReadTarget, bool) {
	target, err := u.ResolveReadTarget(filePath, permissionType)
	return target, err == nil
}

// ResolveReadTarget resolves and authorizes a file path for server-side reads.
// Its error is ErrReadPermissionDenied when the permissions deny the path, and
// otherwise wraps why the path can not be read: os.ErrNotExist for a path
// that does not exist (e.g. a dangling symbolic link or a file removed after
// its glob matched it) and fs.ErrNotRegularFile for a permitted path that is
// no regular file (e.g. a directory). The permissions are checked first, so a
// denied path gives the same error whatever it is; for users the permissions
// apply to, a path that can not be resolved is denied too.
//
// It logs the check at the levels documented for reads, e.g. two info lines
// for a path the OS and a rule permit.
func (u *User) ResolveReadTarget(filePath, permissionType string) (fs.ValidatedReadTarget, error) {
	return u.resolveReadTarget(filePath, permissionType, u.checkLog(false))
}

// ResolveReadTargetQuietly is ResolveReadTarget, with the same result, but
// logs the lines about the path at debug level only (checkLog). It is for
// callers that check many paths at once and log a summary themselves: the
// probe command's field probe and path listing.
func (u *User) ResolveReadTargetQuietly(filePath,
	permissionType string) (fs.ValidatedReadTarget, error) {

	return u.resolveReadTarget(filePath, permissionType, u.checkLog(true))
}

func (u *User) resolveReadTarget(filePath, permissionType string,
	log checkLog) (fs.ValidatedReadTarget, error) {

	log.debug(u, filePath, permissionType, "Checking config permissions")
	if fs.IsJournalSpec(filePath) {
		return u.validateJournalReadTarget(filePath, permissionType, log)
	}

	cleanPath, err := u.cleanReadPath(filePath, permissionType, log)
	if err != nil {
		return fs.ValidatedReadTarget{}, err
	}

	if u.permissionChecked() && !u.hasAnyFilePermission(cleanPath, permissionType, log) {
		return fs.ValidatedReadTarget{}, ErrReadPermissionDenied
	}

	target, err := fs.NewValidatedReadTarget(cleanPath)
	if errors.Is(err, fs.ErrNotRegularFile) {
		// E.g. a directory a glob matched: the read command skips it.
		log.debug(u, cleanPath, permissionType, "Read target is not a regular file", err)
		return fs.ValidatedReadTarget{}, err
	}
	if err != nil {
		log.warn(u, cleanPath, permissionType, "Unable to validate read target", err)
		return fs.ValidatedReadTarget{}, err
	}

	return target, nil
}

// cleanReadPath returns filePath as an absolute path with its symlinks
// evaluated: the path the permissions are checked against.
func (u *User) cleanReadPath(filePath, permissionType string, log checkLog) (string, error) {
	cleanPath, err := filepath.EvalSymlinks(filePath)
	if err != nil {
		log.error(u, filePath, permissionType, "Unable to evaluate symlinks", err)
		if u.permissionChecked() {
			// The permissions of a path that can not be resolved can not
			// be checked; a user gets the same error for it as for any
			// other path the permissions deny.
			return "", ErrReadPermissionDenied
		}
		return "", fmt.Errorf("evaluate symlinks: %w", err)
	}

	cleanPath, err = filepath.Abs(cleanPath)
	if err != nil {
		log.error(u, cleanPath, permissionType, "Unable to make file path absolute", err)
		return "", fmt.Errorf("make file path absolute: %w", err)
	}

	if cleanPath != filePath {
		log.info(u, filePath, cleanPath, permissionType,
			"Calculated new clean path from original file path (possibly symlink)")
	}
	return cleanPath, nil
}

// permissionChecked reports whether the read permissions apply to u: they do
// not to dserver's own scheduled and continuous jobs.
func (u *User) permissionChecked() bool {
	return u.Name != config.ScheduleUser && u.Name != config.ContinuousUser
}

// hasAnyFilePermission reports whether the read permissions permit path.
func (u *User) hasAnyFilePermission(path, permissionType string, log checkLog) bool {
	hasPermission, permissionErr := u.hasFilePermission(path, permissionType, log)
	if permissionErr != nil {
		log.warn(u, path, permissionErr)
	}
	return hasPermission
}

func (u *User) validateJournalReadTarget(spec, permissionType string, log checkLog) (fs.ValidatedReadTarget, error) {
	if u.permissionChecked() {
		hasPermission, permissionErr := u.iteratePaths(spec, permissionType, log)
		if permissionErr != nil {
			log.warn(u, spec, permissionErr)
		}
		if !hasPermission {
			return fs.ValidatedReadTarget{}, ErrReadPermissionDenied
		}
	}

	target, err := fs.NewValidatedJournalTarget(spec)
	if err != nil {
		log.warn(u, spec, permissionType, "Unable to validate journal read target", err)
		return fs.ValidatedReadTarget{}, err
	}
	return target, nil
}

func (u *User) hasFilePermission(cleanPath, permissionType string, log checkLog) (bool, error) {
	// First check file system Linux/UNIX permission.
	if _, err := permissions.ToRead(u.Name, cleanPath, u.log()); err != nil {
		return false, fmt.Errorf("User without OS file system permissions to read path: %w", err)
	}
	log.info(u, cleanPath, permissionType, "User with OS file system permissions to path")

	hasPermission, err := u.iteratePaths(cleanPath, permissionType, log)
	if err != nil {
		return false, err
	}

	return hasPermission, nil
}

// splitPermissionType splits a rule "<type>:<regex>" into its type and its
// regex. The type prefix is optional (default "readfiles"), so a colon alone
// does not make one: only a prefix of lowercase letters is a type. A bare
// regex containing a colon, such as "!^/var/log/(?:secret|audit)/",
// "!^journal:sshd\.service$" or "^/var/log/[[:alpha:]]+$", used to be cut at
// its first colon into an unknown type and was then skipped silently, so a
// bare deny rule failed open and a bare allow rule matched nothing.
func splitPermissionType(permission string) (typeStr, rule string) {
	prefix, rest, ok := strings.Cut(permission, ":")
	if !ok || prefix == "" {
		return "readfiles", permission
	}
	for i := 0; i < len(prefix); i++ {
		if prefix[i] < 'a' || prefix[i] > 'z' {
			return "readfiles", permission
		}
	}
	return prefix, rest
}

// compiledPermissions returns u's permissions compiled, in their order;
// they are compiled once per User, on the first call.
func (u *User) compiledPermissions() []compiledPermission {
	u.compileOnce.Do(func() {
		u.compiled = make([]compiledPermission, 0, len(u.permissions))
		for _, permission := range u.permissions {
			typeStr, rule := splitPermissionType(permission)
			compiled := compiledPermission{typeStr: typeStr, rule: rule}
			compiled.deny = strings.HasPrefix(rule, "!")
			compiled.re, compiled.prog, compiled.err = compilePermission(strings.TrimPrefix(rule, "!"))
			u.compiled = append(u.compiled, compiled)
		}
	})
	return u.compiled
}

// iteratePaths evaluates the user's permission list against cleanPath for the
// given permissionType and returns whether access is granted.
//
// Semantics — "deny wins":
//   - The list is scanned in order.  A rule prefixed with '!' is a deny rule;
//     any other rule is an allow rule.
//   - As soon as a deny rule matches, the function returns false immediately.
//     No later allow rule can override a deny — this prevents misconfigured
//     ACL lists from accidentally granting access to sensitive paths.
//   - If no deny rule matches but at least one allow rule does, access is granted.
//   - If no rule matches at all, access is denied (deny-by-default).
//   - A rule of the type whose regex does not compile fails the check with
//     an error when the scan reaches it.
//   - A path holding a newline is denied with errNewlineInPath before any
//     rule is asked. In a rule "." matches no newline and "$" only the end
//     of the path, so the documented deny form "!^/dir/.*$" does not match
//     "/dir/a\nb" while an allow rule "^/dir/" does: whoever could create
//     such a name beneath a denied directory (or a symlink to it) had the
//     file read past the deny rule. Compiling the rules with "(?s)" would
//     close that for "." only and change what written rules match; "(?m)"
//     rules, classes and rules an operator writes later would still have
//     to think of newlines. Denying the path outright needs no rule to.
//     Log file names hold no newline, so nothing readable is lost.
//
// The rules are compiled once per User (compiledPermissions), not per check.
func (u *User) iteratePaths(cleanPath, permissionType string, log checkLog) (bool, error) {
	// Default: no permission until a matching allow rule is found.
	hasPermission := false
	if strings.ContainsRune(cleanPath, '\n') {
		return false, errNewlineInPath
	}

	for _, permission := range u.compiledPermissions() {
		if log.debugOn {
			log.debug(u, cleanPath, permission.typeStr, permission.rule)
		}
		if permission.typeStr != permissionType {
			continue
		}
		if permission.err != nil {
			return false, permission.err
		}
		if !permission.re.MatchString(cleanPath) {
			continue
		}
		if permission.deny {
			// Deny rule matched: return false immediately (deny wins).
			// A subsequent allow rule must never override an explicit deny.
			if log.infoWritten() {
				log.info(u, cleanPath, "Permission denied: matching deny pattern", permission.rule)
			}
			return false, nil
		}
		if log.infoWritten() {
			log.info(u, cleanPath, "Permission test passed partially, "+
				"matching positive pattern", permission.rule)
		}
		hasPermission = true
	}

	return hasPermission, nil
}

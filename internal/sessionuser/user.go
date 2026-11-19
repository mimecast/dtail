package sessionuser

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

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

// ValidateReadTarget resolves and authorizes a file path for server-side reads.
func (u *User) ValidateReadTarget(filePath, permissionType string) (fs.ValidatedReadTarget, bool) {
	u.log().Debug(u, filePath, permissionType, "Checking config permissions")
	if fs.IsJournalSpec(filePath) {
		return u.validateJournalReadTarget(filePath, permissionType)
	}

	cleanPath, err := filepath.EvalSymlinks(filePath)
	if err != nil {
		u.log().Error(u, filePath, permissionType,
			"Unable to evaluate symlinks", err)
		return fs.ValidatedReadTarget{}, false
	}

	cleanPath, err = filepath.Abs(cleanPath)
	if err != nil {
		u.log().Error(u, cleanPath, permissionType,
			"Unable to make file path absolute", err)
		return fs.ValidatedReadTarget{}, false
	}

	if cleanPath != filePath {
		u.log().Info(u, filePath, cleanPath, permissionType,
			"Calculated new clean path from original file path (possibly symlink)")
	}

	if u.Name != config.ScheduleUser && u.Name != config.ContinuousUser {
		hasPermission, permissionErr := u.hasFilePermission(cleanPath, permissionType)
		if permissionErr != nil {
			u.log().Warn(u, cleanPath, permissionErr)
		}
		if !hasPermission {
			return fs.ValidatedReadTarget{}, false
		}
	}

	target, err := fs.NewValidatedReadTarget(cleanPath)
	if err != nil {
		u.log().Warn(u, cleanPath, permissionType, "Unable to validate read target", err)
		return fs.ValidatedReadTarget{}, false
	}

	return target, true
}

func (u *User) validateJournalReadTarget(spec, permissionType string) (fs.ValidatedReadTarget, bool) {
	if u.Name != config.ScheduleUser && u.Name != config.ContinuousUser {
		hasPermission, permissionErr := u.iteratePaths(spec, permissionType)
		if permissionErr != nil {
			u.log().Warn(u, spec, permissionErr)
		}
		if !hasPermission {
			return fs.ValidatedReadTarget{}, false
		}
	}

	target, err := fs.NewValidatedJournalTarget(spec)
	if err != nil {
		u.log().Warn(u, spec, permissionType, "Unable to validate journal read target", err)
		return fs.ValidatedReadTarget{}, false
	}
	return target, true
}

func (u *User) hasFilePermission(cleanPath, permissionType string) (bool, error) {
	// First check file system Linux/UNIX permission.
	if _, err := permissions.ToRead(u.Name, cleanPath, u.log()); err != nil {
		return false, fmt.Errorf("User without OS file system permissions to read path: %w", err)
	}
	u.log().Info(u, cleanPath, permissionType,
		"User with OS file system permissions to path")

	hasPermission, err := u.iteratePaths(cleanPath, permissionType)
	if err != nil {
		return false, err
	}

	return hasPermission, nil
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
func (u *User) iteratePaths(cleanPath, permissionType string) (bool, error) {
	// Default: no permission until a matching allow rule is found.
	hasPermission := false

	for _, permission := range u.permissions {
		// Determine the permission type prefix; default is "readfiles".
		typeStr := "readfiles"
		splitted := strings.Split(permission, ":")
		if len(splitted) > 1 {
			typeStr = splitted[0]
			permission = strings.Join(splitted[1:], ":")
		}

		u.log().Debug(u, cleanPath, typeStr, permission)
		if typeStr != permissionType {
			continue
		}

		// Detect deny rules (prefixed with '!') and strip the prefix before
		// compiling the regex.
		negate := strings.HasPrefix(permission, "!")
		regexStr := permission
		if negate {
			regexStr = permission[1:]
		}

		re, err := regexp.Compile(regexStr)
		if err != nil {
			return false, fmt.Errorf("permission test failed, can't compile regex "+
				"'%s': %w", regexStr, err)
		}

		if negate && re.MatchString(cleanPath) {
			// Deny rule matched: return false immediately (deny wins).
			// A subsequent allow rule must never override an explicit deny.
			u.log().Info(u, cleanPath, "Permission denied: matching deny pattern", permission)
			return false, nil
		}

		if !negate && re.MatchString(cleanPath) {
			u.log().Info(u, cleanPath, "Permission test passed partially, "+
				"matching positive pattern", permission)
			hasPermission = true
		}
	}

	return hasPermission, nil
}

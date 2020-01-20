package server

import (
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/fs/permissions"
	"github.com/mimecast/dtail/internal/logger"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxLinkDepth int = 100

// User represents an end-user which connected to the server via the DTail client.
type User struct {
	// The user name.
	Name string
	// The remote address connected from.
	remoteAddress string
	// The permissions the user has.
	permissions []string
}

// New returns a new user.
func New(name, remoteAddress string) *User {
	return &User{
		Name:          name,
		remoteAddress: remoteAddress,
	}
}

// String representation of the user.
func (u *User) String() string {
	return fmt.Sprintf("%s@%s", u.Name, u.remoteAddress)
}

// HasFilePermission is used to determine whether user is alowed to read a file.
func (u *User) HasFilePermission(filePath string) (hasPermission bool) {
	cleanPath, err := filepath.EvalSymlinks(filePath)
	if err != nil {
		logger.Error(u, filePath, "Unable to evaluate symlinks", err)
		hasPermission = false
		return
	}

	cleanPath, err = filepath.Abs(cleanPath)
	if err != nil {
		logger.Error(u, cleanPath, "Unable to make file path absolute", err)
		hasPermission = false
		return
	}

	if cleanPath != filePath {
		logger.Info(u, filePath, cleanPath, "Calculated new clean path from original file path (possibly symlink)")
	}

	hasPermission, err = u.hasFilePermission(cleanPath)
	if err != nil {
		logger.Warn(u, cleanPath, err)
	}

	return
}

func (u *User) hasFilePermission(cleanPath string) (bool, error) {
	// First check file system Linux/UNIX permission.
	if _, err := permissions.ToRead(u.Name, cleanPath); err != nil {
		return false, fmt.Errorf("User without OS file system permissions to read file: '%v'", err)
	}
	logger.Info(u, cleanPath, "User has OS file system permissions to read file")

	// If file system permission is given, also check permissions
	// as configured in DTail config file.
	if len(u.permissions) == 0 {
		p, err := config.ServerUserPermissions(u.Name)
		if err != nil {
			return false, err
		}
		u.permissions = p
	}

	var hasPermission bool
	var err error

	if hasPermission, err = u.iteratePaths(cleanPath); err != nil {
		return false, err
	}

	// Only allow to follow regular files or symlinks.
	info, err := os.Lstat(cleanPath)
	if err != nil {
		return false, fmt.Errorf("Unable to determine file type: '%v'", err)
	}

	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("Can only open regular files or follow symlinks")
	}

	return hasPermission, nil
}

func (u *User) iteratePaths(cleanPath string) (bool, error) {
	for _, permission := range u.permissions {
		var regexStr string
		var negate bool

		if strings.HasPrefix(permission, "!") {
			regexStr = permission[1:]
			negate = true
		}
		regexStr = permission
		negate = false

		re, err := regexp.Compile(regexStr)
		if err != nil {
			return false, fmt.Errorf("Permission test failed, can't compile regex '%s': '%v'", regexStr, err)
		}

		if negate && re.MatchString(cleanPath) {
			return false, fmt.Errorf("Permission test failed, matching negative pattern '%s'", permission)
		}

		if !negate && re.MatchString(cleanPath) {
			logger.Info(u, cleanPath, "Permission test passed partially, matching positive pattern", permission)
		}
	}

	return true, nil
}

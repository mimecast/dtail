// +build !linuxacl

package permissions

import (
	"github.com/mimecast/dtail/internal/io/dlog"
)

// ToRead is to check whether user has read permissions to a given file.
func ToRead(user, filePath string) (bool, error) {
	// Only implemented for Linux, always expect true
	dlog.Common.Warn(user, filePath, "Not performing ACL check, not supported on this platform")
	return true, nil
}

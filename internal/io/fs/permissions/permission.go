// +build !linux

package permissions

import (
	"github.com/mimecast/dtail/internal/io/logger"
)

// ToRead is to check whether user has read permissions to a given file.
func ToRead(user, filePath string) (bool, error) {
	// Only implemented for Linux, always expect true
	logger.Warn(user, filePath, "Not performing ACL check, not supported on this platform")
	return true, nil
}

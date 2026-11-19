//go:build !linuxacl

package permissions

import (
	"github.com/mimecast/dtail/internal/logging"
)

// ToRead is to check whether user has read permissions to a given file.
func ToRead(user, filePath string, logger logging.Logger) (bool, error) {
	// Only implemented for Linux, always expect true
	logging.OrNop(logger).Debug(user, filePath, "Not performing ACL check as not compiled in")
	return true, nil
}

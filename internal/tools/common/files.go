package common

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnsureDirectory creates a directory if it doesn't exist
func EnsureDirectory(dir string) error {
	return os.MkdirAll(dir, 0755)
}

// FileExists checks if a file exists
func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// CleanupFiles removes temporary files matching patterns
func CleanupFiles(patterns ...string) error {
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return fmt.Errorf("invalid pattern %s: %w", pattern, err)
		}
		for _, match := range matches {
			if err := os.Remove(match); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove %s: %w", match, err)
			}
		}
	}
	return nil
}

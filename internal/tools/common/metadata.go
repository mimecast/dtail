package common

import (
	"os/exec"
	"strings"
	"time"
)

// GetTimestamp returns a timestamp string for file naming
func GetTimestamp() string {
	return time.Now().Format("20060102_150405")
}

// GetGitCommit returns the current git commit hash (short form)
func GetGitCommit() string {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

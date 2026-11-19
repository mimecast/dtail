package common

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ParseSize parses a size string like "10MB", "1GB" into bytes
func ParseSize(sizeStr string) (int64, error) {
	originalStr := sizeStr
	sizeStr = strings.ToUpper(strings.TrimSpace(sizeStr))

	// Handle single-letter suffixes (K, M, G, T) by adding B
	if len(sizeStr) > 1 {
		lastChar := sizeStr[len(sizeStr)-1]
		secondLastChar := byte('0')
		if len(sizeStr) > 1 {
			secondLastChar = sizeStr[len(sizeStr)-2]
		}

		// If ends with K, M, G, or T and the character before it is a digit, add B
		if (lastChar == 'K' || lastChar == 'M' || lastChar == 'G' || lastChar == 'T') &&
			(secondLastChar >= '0' && secondLastChar <= '9') {
			sizeStr += "B"
		}
	}

	// Order matters - check longer suffixes first
	suffixes := []struct {
		suffix     string
		multiplier int64
	}{
		{"TB", 1024 * 1024 * 1024 * 1024},
		{"GB", 1024 * 1024 * 1024},
		{"MB", 1024 * 1024},
		{"KB", 1024},
		{"B", 1},
	}

	for _, s := range suffixes {
		if strings.HasSuffix(sizeStr, s.suffix) {
			numStr := strings.TrimSuffix(sizeStr, s.suffix)
			numStr = strings.TrimSpace(numStr)
			if numStr == "" {
				return 0, fmt.Errorf("no number before size suffix")
			}
			num, err := strconv.ParseFloat(numStr, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid size number: %s (original: %s, processed: %s)", numStr, originalStr, sizeStr)
			}
			return int64(num * float64(s.multiplier)), nil
		}
	}

	// Try parsing as plain number (assume bytes)
	num, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size format: %s", sizeStr)
	}
	return num, nil
}

// FormatSize formats bytes into human-readable size
func FormatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// BuildCommand builds a dtail command if it doesn't exist
func BuildCommand(cmd string) error {
	// Check if binary exists
	if _, err := os.Stat(cmd); err == nil {
		return nil // Already exists
	}

	// Build the command
	cmdName := filepath.Base(cmd)
	buildCmd := exec.Command("go", "build", "-o", cmd, fmt.Sprintf("./cmd/%s/main.go", cmdName))
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr

	fmt.Printf("Building %s...\n", cmdName)
	return buildCmd.Run()
}

// BuildCommands builds multiple dtail commands
func BuildCommands(commands ...string) error {
	for _, cmd := range commands {
		if err := BuildCommand(cmd); err != nil {
			return fmt.Errorf("failed to build %s: %w", cmd, err)
		}
	}
	return nil
}

// EnsureDirectory creates a directory if it doesn't exist
func EnsureDirectory(dir string) error {
	return os.MkdirAll(dir, 0755)
}

// FileExists checks if a file exists
func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

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

// RunCommandWithTimeout runs a command with a timeout
func RunCommandWithTimeout(timeout time.Duration, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-time.After(timeout):
		if err := cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill process: %w", err)
		}
		return fmt.Errorf("command timed out after %v", timeout)
	case err := <-done:
		return err
	}
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

// Colors for terminal output
const (
	ColorReset  = "\033[0m"
	ColorRed    = "\033[0;31m"
	ColorGreen  = "\033[0;32m"
	ColorYellow = "\033[1;33m"
	ColorBlue   = "\033[0;34m"
	ColorPurple = "\033[0;35m"
	ColorCyan   = "\033[0;36m"
	ColorWhite  = "\033[0;37m"
)

// PrintColored prints colored text to stdout
func PrintColored(color, format string, args ...any) {
	fmt.Printf(color+format+ColorReset, args...)
}

// PrintSection prints a section header
func PrintSection(title string) {
	PrintColored(ColorGreen, "%s\n", title)
	fmt.Println(strings.Repeat("=", len(title)))
}

// PrintInfo prints an info message
func PrintInfo(format string, args ...any) {
	PrintColored(ColorYellow, format, args...)
}

// PrintError prints an error message
func PrintError(format string, args ...any) {
	PrintColored(ColorRed, format, args...)
}

// PrintSuccess prints a success message
func PrintSuccess(format string, args ...any) {
	PrintColored(ColorGreen, format, args...)
}

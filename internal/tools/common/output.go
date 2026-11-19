package common

import (
	"fmt"
	"strings"
)

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

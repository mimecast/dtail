package common

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

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

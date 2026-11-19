package handlers

import (
	"os/exec"
	"runtime"

	"github.com/mimecast/dtail/internal/protocol"
)

// DetectCapabilities reports the capabilities supported by the local process.
// Detection is explicit so importing this package has no host-command side effects.
func DetectCapabilities() []string {
	if runtime.GOOS != "linux" {
		return ServerCapabilities(runtime.GOOS, false)
	}

	_, err := exec.LookPath("journalctl")
	return ServerCapabilities(runtime.GOOS, err == nil)
}

// ServerCapabilities returns capabilities for an operating system and journalctl availability.
func ServerCapabilities(goos string, journalctlAvailable bool) []string {
	capabilities := []string{protocol.CapabilityQueryUpdateV1, protocol.CapabilityCommandFailureV1}
	if goos == "linux" && journalctlAvailable {
		capabilities = append(capabilities, protocol.CapabilityJournalV1)
	}
	return capabilities
}

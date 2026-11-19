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

// ServerCapabilities returns the capabilities for an operating system and
// journalctl availability. Only runtime properties of the server are
// capabilities; what every protocol 5 server supports is not listed (see
// protocol.HiddenCapabilitiesPrefix). The list is empty, never nil, on a
// host without journal support, and the capabilities frame is sent anyway.
func ServerCapabilities(goos string, journalctlAvailable bool) []string {
	capabilities := []string{}
	if goos == "linux" && journalctlAvailable {
		capabilities = append(capabilities, protocol.CapabilityJournalV1)
	}
	return capabilities
}

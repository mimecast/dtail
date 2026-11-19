package version

import (
	"fmt"

	"github.com/mimecast/dtail/internal/protocol"
)

const (
	// Name of DTail.
	Name string = "DTail"
	// Version of DTail.
	Version string = "4.3.2-ng"
	// Additional information for DTail
	Additional string = "Have a lot of fun!"
)

// String representation of the DTail version.
func String() string {
	return fmt.Sprintf("%s %v Protocol %s %s", Name, Version,
		protocol.ProtocolCompat, Additional)
}

package version

import (
	"fmt"
	"os"

	"github.com/mimecast/dtail/internal/color"
)

const (
	// Name of DTail.
	Name string = "DTail"
	// Version of DTail.
	Version string = "2.1.1"
	// Additional information for DTail
	Additional string = ""
	// ProtocolCompat -ibility version.
	ProtocolCompat string = "2"
)

// String representation of the DTail version.
func String() string {
	return fmt.Sprintf("%s %v Protocol %s %s", Name, Version, ProtocolCompat, Additional)
}

// PaintedString is a prettier string representation of the DTail version.
func PaintedString() string {
	if !color.Colored {
		return String()
	}
	name := color.Paint(color.Yellow, Name)
	version := color.Paint(color.Blue, Version)
	descr := color.Paint(color.Green, Additional)

	return fmt.Sprintf("%s %v Protocol %s %s", name, version, ProtocolCompat, descr)
}

// PrintAndExit prints the program version and exists.
func PrintAndExit() {
	fmt.Println(PaintedString())
	os.Exit(0)
}

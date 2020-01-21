package version

import (
	"fmt"
	"os"

	"github.com/mimecast/dtail/internal/color"
)

// Name of DTail.
const Name = "DTail"

// Version of DTail.
const Version = "1.1.0"

// Additional information.
const Additional = ""

// String representation of the DTail version.
func String() string {
	return fmt.Sprintf("%s v%v %s", Name, Version, Additional)
}

// PaintedString is a prettier string representation of the DTail version.
func PaintedString() string {
	if !color.Colored {
		return String()
	}
	name := color.Paint(color.Yellow, Name)
	version := color.Paint(color.Blue, Version)
	descr := color.Paint(color.Green, Additional)

	return fmt.Sprintf("%s %v %s", name, version, descr)
}

// PrintAndExit prints the program version and exists.
func PrintAndExit() {
	fmt.Println(PaintedString())
	os.Exit(0)
}

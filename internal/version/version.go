package version

import (
	"fmt"
	"os"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
)

const (
	// Name of DTail.
	Name string = "DTail"
	// Version of DTail.
	Version string = "3.2.0"
	// ProtocolCompat -ibility version.
	ProtocolCompat string = "3"
	// Additional information for DTail
	Additional string = "Have a lot of fun!"
)

// String representation of the DTail version.
func String() string {
	return fmt.Sprintf("%s %v Protocol %s %s", Name, Version, ProtocolCompat, Additional)
}

// PaintedString is a prettier string representation of the DTail version.
func PaintedString() string {
	if !config.Client.TermColorsEnabled {
		return String()
	}

	name := color.PaintWithAttr(Name,
		color.FgYellow, color.BgBlue, color.AttrBold)

	version := color.PaintWithAttr(fmt.Sprintf(" %s ", Version),
		color.FgBlue, color.BgYellow, color.AttrBold)

	protocol := color.Paint(fmt.Sprintf(" Protocol %s ", ProtocolCompat),
		color.FgBlack, color.BgGreen)

	additional := color.PaintWithAttr(fmt.Sprintf(" %s ", Additional),
		color.FgWhite, color.BgMagenta, color.AttrBlink)

	return fmt.Sprintf("%s%v%s%s", name, version, protocol, additional)
}

// PrintAndExit prints the program version and exists.
func PrintAndExit() {
	fmt.Println(PaintedString())
	os.Exit(0)
}

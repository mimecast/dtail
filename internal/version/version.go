package version

import (
	"fmt"
	"os"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/protocol"
)

const (
	// Name of DTail.
	Name string = "DTail"
	// Version of DTail.
	Version string = "4.3.0"
	// Additional information for DTail
	Additional string = "Have a lot of fun!"
)

// String representation of the DTail version.
func String() string {
	return fmt.Sprintf("%s %v Protocol %s %s", Name, Version,
		protocol.ProtocolCompat, Additional)
}

// PaintedString is a prettier string representation of the DTail version.
func PaintedString() string {
	if !config.Client.TermColorsEnable {
		return String()
	}

	name := color.PaintStrWithAttr(fmt.Sprintf(" %s ", Name),
		color.FgYellow, color.BgBlue, color.AttrBold)
	version := color.PaintStrWithAttr(fmt.Sprintf(" %s ", Version),
		color.FgBlue, color.BgYellow, color.AttrBold)
	protocol := color.PaintStr(fmt.Sprintf(" Protocol %s ", protocol.ProtocolCompat),
		color.FgBlack, color.BgGreen)
	additional := color.PaintStrWithAttr(fmt.Sprintf(" %s ", Additional),
		color.FgWhite, color.BgMagenta, color.AttrUnderline)

	return fmt.Sprintf("%s%v%s%s", name, version, protocol, additional)
}

// Print the version.
func Print() {
	fmt.Println(PaintedString())
}

// PrintAndExit prints the program version and exists.
func PrintAndExit() {
	Print()
	os.Exit(0)
}

package cli

import (
	"fmt"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/version"
)

func versionString(colorsEnabled bool) string {
	if !colorsEnabled {
		return version.String()
	}

	name := color.PaintStrWithAttr(fmt.Sprintf(" %s ", version.Name),
		color.FgYellow, color.BgBlue, color.AttrBold)
	versionNumber := color.PaintStrWithAttr(fmt.Sprintf(" %s ", version.Version),
		color.FgBlue, color.BgYellow, color.AttrBold)
	protocolVersion := color.PaintStr(fmt.Sprintf(" Protocol %s ", protocol.ProtocolCompat),
		color.FgBlack, color.BgGreen)
	additional := color.PaintStrWithAttr(fmt.Sprintf(" %s ", version.Additional),
		color.FgWhite, color.BgMagenta, color.AttrUnderline)

	return fmt.Sprintf("%s%v%s%s", name, versionNumber, protocolVersion, additional)
}

// PrintVersion writes the formatted DTail version to standard output.
func PrintVersion(colorsEnabled bool) {
	fmt.Println(versionString(colorsEnabled))
}

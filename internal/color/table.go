package color

import (
	"fmt"
	"os"
)

var ColorNames = []string{
	"Black", "Red", "Green", "Yellow", "Blue", "Magenta", "Cyan", "White", "Default",
}

var AttributeNames = []string{
	"Bold", "Dim", "Italic", "Underline", "Blink", "SlowBlink", "RapidBlink", "Reverse", "Hidden", "None",
}

func TablePrintAndExit() {
	for _, attr := range AttributeNames {
		if attr == "Hidden" || attr == "SlowBlink" {
			continue
		}
		printColorTable(attr)
	}
	os.Exit(0)
}

func printColorTable(attr string) {
	for _, fg := range ColorNames {
		fgColor, _ := ToFgColor(fg)
		for _, bg := range ColorNames {
			if fg == bg {
				continue
			}
			bgColor, _ := ToBgColor(bg)
			attribute, _ := ToAttribute(attr)

			text := fmt.Sprintf(" Foreground:%10s  |  Background:%10s  |  Attribute:%10s ", fg, bg, attr)
			fmt.Print(PaintWithAttr(text, fgColor, bgColor, attribute))
			fmt.Print("\n")
		}
	}
}

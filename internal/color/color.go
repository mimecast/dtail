// Package color is used to prettify console output via ANSII terminal colors.
package color

import (
	"fmt"
)

// Color name.
type Color string

// Attribute of a color.
type Attribute string

// The possible color variations.
const (
	escape        = "\x1b"
	reset         = escape + "[0m"
	seq    string = "%s%s%s"

	Gray      Color = escape + "[30m"
	Red       Color = escape + "[31m"
	Green     Color = escape + "[32m"
	Orange    Color = escape + "[33m"
	Blue      Color = escape + "[34m"
	Magenta   Color = escape + "[35m"
	Yellow    Color = escape + "[36m"
	LightGray Color = escape + "[37m"

	BgGray      Color = escape + "[40m" BgRed       Color = escape + "[41m"
	BgGreen     Color = escape + "[42m"
	BgOrange    Color = escape + "[43m"
	BgBlue      Color = escape + "[44m"
	BgMagenta   Color = escape + "[45m"
	BgYellow    Color = escape + "[46m"
	BgLightGray Color = escape + "[47m"

	Bold         Attribute = escape + "[1m"
	Italic       Attribute = escape + "[3m"
	Underline    Attribute = escape + "[4m"
	ReverseColor Attribute = escape + "[7m"

	resetBold      = escape + "[22m"
	resetItalic    = escape + "[23m"
	resetUnderline = escape + "[24m"

	Test     Color     = BgYellow
	TestAttr Attribute = Bold
)

// Colored DTail client output enabled.
var Colored bool

// Paint a given string in a given color.
func Paint(c Color, s string) string {
	return fmt.Sprintf(seq, c, s, reset)
}

// Attr adds a given attribute to a given string, such as "bold" or "italic".
func Attr(c Attribute, s string) string {
	switch c {
	case Bold:
		return fmt.Sprintf(seq, Bold, s, resetBold)
	case Italic:
		return fmt.Sprintf(seq, Italic, s, resetItalic)
	case Underline:
		return fmt.Sprintf(seq, Underline, s, resetUnderline)
	}
	panic("Unknown attribute")
}

// Package color contains all terminal color codes we know of.
package color

import (
	"fmt"
	"strings"
)

// FgColor is the text foreground color.
type FgColor string

// BgColor is the text background color.
type BgColor string

// Attribute of text.
type Attribute string

// The possible color variations.
const (
	escape = "\x1b"

	FgBlack   FgColor = escape + "[30m"
	FgRed     FgColor = escape + "[31m"
	FgGreen   FgColor = escape + "[32m"
	FgYellow  FgColor = escape + "[33m"
	FgBlue    FgColor = escape + "[34m"
	FgMagenta FgColor = escape + "[35m"
	FgCyan    FgColor = escape + "[36m"
	FgWhite   FgColor = escape + "[37m"
	FgDefault FgColor = escape + "[39m"

	BgBlack   BgColor = escape + "[40m"
	BgRed     BgColor = escape + "[41m"
	BgGreen   BgColor = escape + "[42m"
	BgYellow  BgColor = escape + "[43m"
	BgBlue    BgColor = escape + "[44m"
	BgMagenta BgColor = escape + "[45m"
	BgCyan    BgColor = escape + "[46m"
	BgWhite   BgColor = escape + "[47m"
	BgDefault BgColor = escape + "[49m"

	AttrNone       Attribute = ""
	AttrReset      Attribute = escape + "[0m"
	AttrBold       Attribute = escape + "[1m"
	AttrDim        Attribute = escape + "[2m"
	AttrItalic     Attribute = escape + "[3m"
	AttrUnderline  Attribute = escape + "[4m"
	AttrBlink      Attribute = escape + "[5m"
	AttrSlowBlink  Attribute = escape + "[5m"
	AttrRapidBlink Attribute = escape + "[6m"
	AttrReverse    Attribute = escape + "[7m"
	AttrHidden     Attribute = escape + "[8m"
)

var ColorNames = []string{
	"Black", "Red", "Green", "Yellow", "Blue", "Magenta", "Cyan", "White", "Default",
}

var AttributeNames = []string{
	"Bold", "Dim", "Italic", "Underline", "Blink", "SlowBlink", "RapidBlink", "Reverse", "Hidden", "None",
}

// ToFgColor converts a given string (e.g. from a config file) into a foreground color code.
func ToFgColor(s string) (FgColor, error) {
	switch strings.ToLower(s) {
	case "black":
		return FgBlack, nil
	case "red":
		return FgRed, nil
	case "green":
		return FgGreen, nil
	case "yellow":
		return FgYellow, nil
	case "blue":
		return FgBlue, nil
	case "magenta":
		return FgMagenta, nil
	case "cyan":
		return FgCyan, nil
	case "white":
		return FgWhite, nil
	case "default":
		return FgDefault, nil
	default:
		return FgDefault, fmt.Errorf("unknown foreground text color '" + s + "'")
	}
}

// ToBgColor converts a given string (e.g. from a config file) into a background color code.
func ToBgColor(s string) (BgColor, error) {
	switch strings.ToLower(s) {
	case "black":
		return BgBlack, nil
	case "red":
		return BgRed, nil
	case "green":
		return BgGreen, nil
	case "yellow":
		return BgYellow, nil
	case "blue":
		return BgBlue, nil
	case "magenta":
		return BgMagenta, nil
	case "cyan":
		return BgCyan, nil
	case "white":
		return BgWhite, nil
	case "default":
		return BgDefault, nil
	default:
		return BgDefault, fmt.Errorf("unknown background text color '" + s + "'")
	}
}

// ToAttribute converts a given string (e.g. from a config file) into a text attribute.
func ToAttribute(s string) (Attribute, error) {
	switch strings.ToLower(s) {
	case "bold":
		return AttrBold, nil
	case "dim":
		return AttrDim, nil
	case "italic":
		return AttrItalic, nil
	case "underline":
		return AttrUnderline, nil
	case "blink":
		return AttrBlink, nil
	case "slowblink":
		return AttrSlowBlink, nil
	case "rapidblink":
		return AttrRapidBlink, nil
	case "reverse":
		return AttrReverse, nil
	case "hidden":
		return AttrHidden, nil
	case "none":
		fallthrough
	case "":
		return AttrNone, nil
	default:
		return AttrReset, fmt.Errorf("unknown text attribute '" + s + "'")
	}
}

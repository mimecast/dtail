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
	escape        = "\x1b"
	seq    string = "%s%s%s"

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

	AttReset      Attribute = escape + "[0m"
	AttBold       Attribute = escape + "[1m"
	AttDim        Attribute = escape + "[2m"
	AttItalic     Attribute = escape + "[3m"
	AttUnderline  Attribute = escape + "[4m"
	AttBlink      Attribute = escape + "[5m"
	AttSlowBlink  Attribute = escape + "[5m"
	AttRapidBlink Attribute = escape + "[6m"
	AttReverse    Attribute = escape + "[7m"
	AttHidden     Attribute = escape + "[8m"

	// Internal (manual) testing.
	FgTest  FgColor   = FgBlue
	BgTest  BgColor   = BgYellow
	AttTest Attribute = AttBold
)

// Colored DTail client output enabled.
var Colored bool

// Paint paints a given text in a given foreground/background color combination.
func Paint(text string, fg FgColor, bg FgColor) string {
	return fmt.Sprintf(seq, fg, bg, text, BgDefault, FgDefault)
}

// PaintWithAttr paints a given text in a given foreground/background/attribute combination
func PaintWithAtt(text string, fg FgColor, bg FgColor, att Attribute) string {
	return fmt.Sprintf(seq, fg, bg, att, text, AttReset, BgDefault, FgDefault)
}

// PaintFg paints a given text in a given foreground color.
func PaintFg(text string, fg FgColor) string {
	return fmt.Sprintf(seq, fg, text, FgDefault)
}

// PaintBg paints a given text in a given background color.
func PaintBg(text string, bg BgColor) string {
	return fmt.Sprintf(seq, bg, text, BgDefault)
}

// PaintAtt adds a given attribute to a given text, such as "bold" or "italic".
func PaintAtt(text string, att Attribute) string {
	return fmt.Sprintf(seq, att, text, AttReset)
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
func ToAttColor(s string) (Attribute, error) {
	switch strings.ToLower(s) {
	case "bold":
		return AttBold, nil
	case "dim":
		return AttDim, nil
	case "italic":
		return AttItalic, nil
	case "underline":
		return AttUnderline, nil
	case "blink":
		return AttBlink, nil
	case "slowblink":
		return AttSlowBlink, nil
	case "rapidblink":
		return AttRapidBlink, nil
	case "reverse":
		return AttReverse, nil
	case "hidden":
		return AttHidden, nil
	default:
		return AttReset, fmt.Errorf("unknown text attribute '" + s + "'")
	}
}

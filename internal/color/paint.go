package color

import "fmt"

// Paint paints a given text in a given foreground/background color combination.
func Paint(text string, fg FgColor, bg BgColor) string {
	return fmt.Sprintf("%s%s%s%s%s", fg, bg, text, BgDefault, FgDefault)
}

// PaintWithAttr paints a given text in a given foreground/background/attribute combination
func PaintWithAttr(text string, fg FgColor, bg BgColor, attr Attribute) string {
	if attr == AttrNone {
		return Paint(text, fg, bg)
	}
	return fmt.Sprintf("%s%s%s%s%s%s%s", fg, bg, attr, text, AttrReset, BgDefault, FgDefault)
}

// PaintFg paints a given text in a given foreground color.
func PaintFg(text string, fg FgColor) string {
	return fmt.Sprintf("%s%s%s", fg, text, FgDefault)
}

// PaintBg paints a given text in a given background color.
func PaintBg(text string, bg BgColor) string {
	return fmt.Sprintf("%s%s%s", bg, text, BgDefault)
}

// PaintAttr adds a given attribute to a given text, such as "bold" or "italic".
func PaintAttr(text string, attr Attribute) string {
	return fmt.Sprintf("%s%s%s", attr, text, AttrReset)
}

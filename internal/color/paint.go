package color

import (
	"fmt"
	"strings"
)

// PaintStr paints a given text in a given foreground/background color combination.
func PaintStr(text string, fg FgColor, bg BgColor) string {
	return fmt.Sprintf("%s%s%s%s%s", fg, bg, text, BgDefault, FgDefault)
}

// PaintStrWithAttr paints a given text in a given foreground/background/attribute combination
func PaintStrWithAttr(text string, fg FgColor, bg BgColor, attr Attribute) string {
	if attr == AttrNone {
		return PaintStr(text, fg, bg)
	}
	return fmt.Sprintf("%s%s%s%s%s%s%s", fg, bg, attr, text, AttrReset, BgDefault, FgDefault)
}

// PaintStrFg paints a given text in a given foreground color.
func PaintStrFg(text string, fg FgColor) string {
	return fmt.Sprintf("%s%s%s", fg, text, FgDefault)
}

// PaintStrBg paints a given text in a given background color.
func PaintStrBg(text string, bg BgColor) string {
	return fmt.Sprintf("%s%s%s", bg, text, BgDefault)
}

// PaintStrAttr adds a given attribute to a given text, such as "bold" or "italic".
func PaintStrAttr(text string, attr Attribute) string {
	return fmt.Sprintf("%s%s%s", attr, text, AttrReset)
}

// Paint paints a given text in a given foreground/background color combination.
func Paint(sb *strings.Builder, text string, fg FgColor, bg BgColor) {
	sb.WriteString(string(fg))
	sb.WriteString(string(bg))
	sb.WriteString(text)
	sb.WriteString(string(BgDefault))
	sb.WriteString(string(FgDefault))
}

// Reset background and foreground colors.
func Reset(sb *strings.Builder) {
	sb.WriteString(string(BgDefault))
	sb.WriteString(string(FgDefault))
}

// PaintWithAttr starts painting a given text in a given foreground/background/attribute combination.
func PaintWithAttr(sb *strings.Builder, text string, fg FgColor, bg BgColor, attr Attribute) {
	if attr == AttrNone {
		Paint(sb, text, fg, bg)
		return
	}
	sb.WriteString(string(fg))
	sb.WriteString(string(bg))
	sb.WriteString(string(attr))
	sb.WriteString(text)
	sb.WriteString(string(AttrReset))
	sb.WriteString(string(BgDefault))
	sb.WriteString(string(FgDefault))
}

// PaintWithAttrs is similar to PaintWithAttr, but it takes multiple text attributes.
func PaintWithAttrs(sb *strings.Builder, text string, fg FgColor, bg BgColor, attrs []Attribute) {
	sb.WriteString(string(fg))
	sb.WriteString(string(bg))
	for _, attr := range attrs {
		sb.WriteString(string(attr))
	}
	sb.WriteString(text)
	sb.WriteString(string(AttrReset))
	sb.WriteString(string(BgDefault))
	sb.WriteString(string(FgDefault))
}

// ResetWithAttr resets background, foreground and attributes.
func ResetWithAttr(sb *strings.Builder) {
	sb.WriteString(string(AttrReset))
	sb.WriteString(string(BgDefault))
	sb.WriteString(string(FgDefault))
}

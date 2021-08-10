package color

import (
	"strings"
	"testing"
)

func TestColors(t *testing.T) {
	colors := []string{
		"Black", "Red", "Green", "Yellow", "Blue", "Magenta", "Cyan", "White", "Default",
	}

	text := " Mimecast "
	builder := strings.Builder{}

	for _, color := range colors {
		fgColor, err := ToFgColor(color)
		if err != nil {
			t.Errorf("unable to paint foreground : %s\n%v", text, err)
		}
		builder.WriteString(PaintFg(text, fgColor))

		bgColor, err := ToBgColor(color)
		if err != nil {
			t.Errorf("unable to paint background: %s\n%v", text, err)
		}
		builder.WriteString(PaintBg(text, bgColor))
	}

	for _, color := range colors {
		fgColor, _ := ToFgColor(color)
		for _, color := range colors {
			bgColor, _ := ToBgColor(color)
			builder.WriteString(Paint(text, fgColor, bgColor))
		}
	}

	t.Log(builder.String())
}
func TestAttributes(t *testing.T) {
	attributes := []string{
		"Bold", "Dim", "Italic", "Underline", "Blink", "SlowBlink", "RapidBlink", "Reverse", "hidden",
	}

	text := " Mimecast "
	builder := strings.Builder{}

	for _, attribute := range attributes {
		att, err := ToAttribute(attribute)
		if err != nil {
			t.Errorf("unable to paint attribute: %s\n%v", text, err)
		}
		builder.WriteString(PaintWithAttr(text, FgWhite, BgBlue, att))
	}

	t.Log(builder.String())
}

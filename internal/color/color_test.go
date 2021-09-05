package color

import (
	"strings"
	"testing"
)

func TestColors(t *testing.T) {
	text := " Mimecast "
	builder := strings.Builder{}

	for _, color := range ColorNames {
		fgColor, err := ToFgColor(color)
		if err != nil {
			t.Errorf("unable to paint foreground : %s\n%v", text, err)
		}
		builder.WriteString(PaintStrFg(text, fgColor))

		bgColor, err := ToBgColor(color)
		if err != nil {
			t.Errorf("unable to paint background: %s\n%v", text, err)
		}
		builder.WriteString(PaintStrBg(text, bgColor))
	}

	for _, fg := range ColorNames {
		fgColor, _ := ToFgColor(fg)
		for _, bg := range ColorNames {
			if fg == bg {
				continue
			}
			bgColor, _ := ToBgColor(bg)
			builder.WriteString(PaintStr(text, fgColor, bgColor))
		}
	}

	t.Log(builder.String())
}

func TestAttributes(t *testing.T) {
	text := " Mimecast "
	builder := strings.Builder{}

	for _, attribute := range AttributeNames {
		att, err := ToAttribute(attribute)
		if err != nil {
			t.Errorf("unable to paint attribute: %s\n%v", text, err)
		}
		builder.WriteString(PaintStrWithAttr(text, FgWhite, BgBlue, att))
	}

	t.Log(builder.String())
}

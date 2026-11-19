package color

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUnmarshalFgColor(t *testing.T) {
	tests := []struct {
		input    string
		want     FgColor
		wantErr  string
		wantWarn string
	}{
		{input: `"Black"`, want: FgBlack},
		{input: `"Red"`, want: FgRed},
		{input: `"Green"`, want: FgGreen},
		{input: `"Yellow"`, want: FgYellow},
		{input: `"Blue"`, want: FgBlue},
		{input: `"Magenta"`, want: FgMagenta},
		{input: `"Cyan"`, want: FgCyan},
		{input: `"White"`, want: FgWhite},
		{input: `"Default"`, want: FgDefault},
		{input: `"red"`, want: FgRed},
		{input: `"WHITE"`, want: FgWhite},
		{input: `"mAgEnTa"`, want: FgMagenta},
		{input: `"\u001b[37m"`, want: FgWhite},
		{input: `"\u001b[38;5;208m"`, want: FgColor("\x1b[38;5;208m")},
		{input: `"\u001b[38:5:208m"`, want: FgColor("\x1b[38:5:208m")},
		{input: `"\u001b[31m\u001b[1m"`, want: FgColor("\x1b[31m\x1b[1m")},
		{input: `""`, want: FgColor("")},
		{input: `"FgBlack"`, want: FgBlack},
		{input: `"fgwhite"`, want: FgWhite},
		{input: `"FGCyan"`, want: FgCyan},
		{input: `"BgCyan"`, want: FgCyan},
		{input: `"Purple"`, want: FgCyan, wantWarn: `invalid foreground color "Purple", using the default instead: must be one of Black, Red`},
		{input: `"Fg"`, want: FgCyan, wantWarn: `invalid foreground color "Fg"`},
		{input: `"AttrRed"`, want: FgCyan, wantWarn: `invalid foreground color "AttrRed"`},
		{input: `"\u001b[37"`, want: FgCyan, wantWarn: `invalid foreground color`},
		{input: `"Red\u001b[37m"`, want: FgCyan, wantWarn: `invalid foreground color`},
		{input: `37`, want: FgCyan, wantErr: `invalid foreground color 37: must be a string`},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := FgCyan // The default a failed decode must keep.
			checkUnmarshal(t, tt.input, &got, tt.wantErr, tt.wantWarn)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUnmarshalBgColor(t *testing.T) {
	tests := []struct {
		input    string
		want     BgColor
		wantErr  string
		wantWarn string
	}{
		{input: `"Black"`, want: BgBlack},
		{input: `"Red"`, want: BgRed},
		{input: `"Green"`, want: BgGreen},
		{input: `"Yellow"`, want: BgYellow},
		{input: `"Blue"`, want: BgBlue},
		{input: `"Magenta"`, want: BgMagenta},
		{input: `"Cyan"`, want: BgCyan},
		{input: `"White"`, want: BgWhite},
		{input: `"Default"`, want: BgDefault},
		{input: `"blue"`, want: BgBlue},
		{input: `"DEFAULT"`, want: BgDefault},
		{input: `"\u001b[44m"`, want: BgBlue},
		{input: `"\u001b[48:2:0:0:95m"`, want: BgColor("\x1b[48:2:0:0:95m")},
		{input: `"\u001b[44m\u001b[5m"`, want: BgColor("\x1b[44m\x1b[5m")},
		{input: `""`, want: BgColor("")},
		{input: `"BgCyan"`, want: BgCyan},
		{input: `"bgMAGENTA"`, want: BgMagenta},
		{input: `"FgRed"`, want: BgRed},
		{input: `"Pink"`, want: BgGreen, wantWarn: `invalid background color "Pink", using the default instead: must be one of Black, Red`},
		{input: `"Bg"`, want: BgGreen, wantWarn: `invalid background color "Bg"`},
		{input: `true`, want: BgGreen, wantErr: `must be a string`},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := BgGreen // The default a failed decode must keep.
			checkUnmarshal(t, tt.input, &got, tt.wantErr, tt.wantWarn)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUnmarshalAttribute(t *testing.T) {
	tests := []struct {
		input    string
		want     Attribute
		wantErr  string
		wantWarn string
	}{
		{input: `"None"`, want: AttrNone},
		{input: `""`, want: AttrNone},
		{input: `"Bold"`, want: AttrBold},
		{input: `"Dim"`, want: AttrDim},
		{input: `"Italic"`, want: AttrItalic},
		{input: `"Underline"`, want: AttrUnderline},
		{input: `"Blink"`, want: AttrBlink},
		{input: `"SlowBlink"`, want: AttrSlowBlink},
		{input: `"RapidBlink"`, want: AttrRapidBlink},
		{input: `"Reverse"`, want: AttrReverse},
		{input: `"Hidden"`, want: AttrHidden},
		{input: `"bold"`, want: AttrBold},
		{input: `"rapidBLINK"`, want: AttrRapidBlink},
		{input: `"\u001b[2m"`, want: AttrDim},
		{input: `"\u001b[0m"`, want: AttrReset},
		{input: `"\u001b[1m\u001b[4m"`, want: Attribute("\x1b[1m\x1b[4m")},
		{input: `"AttrDim"`, want: AttrDim},
		{input: `"attrbold"`, want: AttrBold},
		{input: `"AttrNone"`, want: AttrNone},
		{input: `"ATTRReverse"`, want: AttrReverse},
		{input: `"Strike"`, want: AttrHidden, wantWarn: `invalid text attribute "Strike", using the default instead: must be one of Bold, Dim`},
		{input: `"FgBold"`, want: AttrHidden, wantWarn: `invalid text attribute "FgBold"`},
		{input: `[]`, want: AttrHidden, wantErr: `must be a string`},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := AttrHidden // The default a failed decode must keep.
			checkUnmarshal(t, tt.input, &got, tt.wantErr, tt.wantWarn)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestUnmarshalNullKeepsValue checks that a JSON null leaves the current
// (default) value in place, as encoding/json does for plain string fields.
func TestUnmarshalNullKeepsValue(t *testing.T) {
	var palette struct {
		Fg   FgColor
		Bg   BgColor
		Attr Attribute
	}
	palette.Fg, palette.Bg, palette.Attr = FgCyan, BgBlue, AttrBold
	if err := json.Unmarshal([]byte(`{"Fg": null, "Bg": null, "Attr": null}`), &palette); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if palette.Fg != FgCyan || palette.Bg != BgBlue || palette.Attr != AttrBold {
		t.Fatalf("null overwrote the defaults: %q %q %q", palette.Fg, palette.Bg, palette.Attr)
	}
}

// TestUnmarshalRoundTrip checks that a palette marshalled from escape codes
// decodes back to the same codes through the raw escape sequence path.
func TestUnmarshalRoundTrip(t *testing.T) {
	type palette struct {
		Fg   FgColor
		Bg   BgColor
		Attr Attribute
	}
	want := palette{Fg: FgYellow, Bg: BgMagenta, Attr: AttrUnderline}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got palette
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// checkUnmarshal decodes input into target and checks the returned error and
// the recorded config warning against the wanted substrings ("" for none).
func checkUnmarshal(t *testing.T, input string, target any, wantErr, wantWarn string) {
	t.Helper()
	TakeConfigWarnings()
	err := json.Unmarshal([]byte(input), target)
	warnings := TakeConfigWarnings()
	switch {
	case wantErr == "" && err != nil:
		t.Fatalf("unexpected error: %v", err)
	case wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)):
		t.Fatalf("error %v does not contain %q", err, wantErr)
	}
	if wantWarn == "" {
		if len(warnings) != 0 {
			t.Fatalf("unexpected warnings: %q", warnings)
		}
		return
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], wantWarn) {
		t.Fatalf("warnings %q do not contain exactly one %q", warnings, wantWarn)
	}
}

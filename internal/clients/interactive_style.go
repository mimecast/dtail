package clients

import (
	"io"
	"strings"
	"unicode/utf8"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
)

// interactiveStyle paints the interactive control output with the
// TermColors.Interactive theme: messages, :help, :show, :fields, the prompt
// and Tab completion candidates. A nil style paints nothing, so every method
// returns its text unchanged: that is the style of the plain line reading
// (scannerLines), of --noColor and --plain (Client.TermColorsEnable false)
// and of every writer that is not the line editor's (tests write to
// buffers).
type interactiveStyle struct {
	theme config.InteractiveTermColors
}

// newInteractiveStyle returns the style of the line editor for the client
// configuration cfg: nil without one or when terminal colors are disabled.
func newInteractiveStyle(cfg *config.ClientConfig) *interactiveStyle {
	if cfg == nil || !cfg.TermColorsEnable {
		return nil
	}
	return &interactiveStyle{theme: cfg.TermColors.Interactive}
}

// interactiveStyle returns the style of the client's line editor, from the
// client configuration of its runtime.
func (c *baseClient) interactiveStyle() *interactiveStyle {
	if c.runtime == nil {
		return nil
	}
	return newInteractiveStyle(c.runtime.clientCfg)
}

// styledWriter is a control output writer that carries the style its
// output is painted with. The line editor's writer is one.
type styledWriter interface {
	io.Writer
	style() *interactiveStyle
}

// styleOf returns the style of the control output writer w: nil (no colors)
// unless w is a styledWriter.
func styleOf(w io.Writer) *interactiveStyle {
	if sw, ok := w.(styledWriter); ok {
		return sw.style()
	}
	return nil
}

// editorWriter is the line editor's control output writer: the Terminal
// (which clears and redraws the line being edited around every write) with
// the editor's style.
type editorWriter struct {
	io.Writer
	styled *interactiveStyle
}

func (w editorWriter) style() *interactiveStyle { return w.styled }

// paint returns text in style ts, or text unchanged for a nil style, an
// empty text or a style without color and attribute. Every attribute and
// the color are reset at the end of text (a raw SGR value in the config,
// e.g. "\u001b[1;33m" as Fg, may set an attribute too), so painted parts
// can be joined freely and nothing leaks into the edit line. A text of
// several lines (e.g. an errors.Join error after "reload failed: ") is
// painted line by line, as help and fields do, so no style runs across a
// line end and empty lines stay empty.
func (s *interactiveStyle) paint(ts config.TermStyle, text string) string {
	if s == nil || text == "" || (ts.Fg == "" && ts.Attr == "") {
		return text
	}
	if strings.Contains(text, "\n") {
		lines := strings.Split(text, "\n")
		for i, line := range lines {
			lines[i] = s.paint(ts, line)
		}
		return strings.Join(lines, "\n")
	}
	return string(ts.Fg) + string(ts.Attr) + text + string(color.AttrReset) + string(color.FgDefault)
}

// The kinds of control messages, each painted in its theme style.
func (s *interactiveStyle) banner(text string) string  { return s.paint(s.themeOf().Banner, text) }
func (s *interactiveStyle) hint(text string) string    { return s.paint(s.themeOf().Hint, text) }
func (s *interactiveStyle) paused(text string) string  { return s.paint(s.themeOf().Paused, text) }
func (s *interactiveStyle) success(text string) string { return s.paint(s.themeOf().Success, text) }
func (s *interactiveStyle) failure(text string) string { return s.paint(s.themeOf().Error, text) }

// themeOf returns the theme of s, the zero theme for nil (paint then
// returns its text unchanged anyway).
func (s *interactiveStyle) themeOf() config.InteractiveTermColors {
	if s == nil {
		return config.InteractiveTermColors{}
	}
	return s.theme
}

// prompt returns the prompt of the line editor while the output is paused.
func (s *interactiveStyle) prompt() string {
	return s.paint(s.themeOf().Prompt, interactivePrompt)
}

// editorPrompt returns the prompt of a line the line editor starts on a
// terminal width columns wide: none while the output runs (paused false),
// else the painted prompt, or the plain interactivePrompt where the
// terminal is narrower than the painted prompt has runes. x/term's
// Terminal cuts the prompt into rows of width runes, escape sequences
// included, and where a cut falls into a sequence it counts the cursor one
// column off (seen at widths 17-19 and 21-24 with the default theme).
func (s *interactiveStyle) editorPrompt(paused bool, width int) string {
	if !paused {
		return ""
	}
	prompt := s.prompt()
	if utf8.RuneCountInString(prompt) > width {
		return interactivePrompt
	}
	return prompt
}

// interactivePrompt is shown in front of a line started while the output
// is paused (interactiveEditor.setPrompt); while the output runs the prompt
// is empty, as streaming lines and a prompt would overwrite each other.
const interactivePrompt = "dtail> "

// help paints the :help text (interactiveHelpText) line by line: the line
// naming the commands and the lines introducing examples as headings, each
// example's command as a command and its explanation as an example.
func (s *interactiveStyle) help(text string) string {
	if s == nil {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "Commands:") || strings.HasSuffix(line, "e.g.:"):
			lines[i] = s.paint(s.theme.Heading, line)
		case strings.HasPrefix(line, "  :"):
			lines[i] = s.helpExample(line)
		}
	}
	return strings.Join(lines, "\n")
}

// helpExample paints one example line of helpExamples: the command up to
// the blanks aligning the explanation, and the explanation.
func (s *interactiveStyle) helpExample(line string) string {
	command, explanation := line, ""
	if i := strings.Index(line[2:], "  "); i >= 0 {
		command, explanation = line[:i+2], line[i+2:]
	}
	gap := explanation[:len(explanation)-len(strings.TrimLeft(explanation, " "))]
	return "  " + s.paint(s.theme.Command, command[2:]) + gap +
		s.paint(s.theme.Example, strings.TrimLeft(explanation, " "))
}

// keyValue is a key=value pair of :show. A painted value (the highlighted
// query) is written as it is, any other one in the theme's value style.
type keyValue struct {
	key, value string
	painted    bool
}

// keyValues joins the key=value pairs of :show, each key and value painted.
func (s *interactiveStyle) keyValues(pairs []keyValue) string {
	parts := make([]string, len(pairs))
	for i, pair := range pairs {
		value := pair.value
		if !pair.painted {
			value = s.paint(s.themeOf().Value, value)
		}
		parts[i] = s.paint(s.themeOf().Key, pair.key) + "=" + value
	}
	return strings.Join(parts, " ")
}

// fields paints the :fields answer (fieldsText) line by line: the answer
// without field data (fieldsEmptyMessage) as a hint, else the header
// and the log format and table lines as headings, each field's name as a
// key and its sample values as values, and probe errors as errors. Every
// text from a server in it is already escaped (displayText), so painting
// whole lines and parts of them cannot combine with sequences of its own.
func (s *interactiveStyle) fields(text string) string {
	if s == nil {
		return text
	}
	if text == fieldsEmptyMessage {
		// No catalog yet: a hint what to do, not a catalog's heading.
		return s.paint(s.theme.Hint, text)
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		switch {
		case i == 0 || strings.HasPrefix(line, "log format ") ||
			(strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, fieldsSampleIndent) &&
				strings.HasSuffix(line, ":")):
			lines[i] = s.paint(s.theme.Heading, line)
		case strings.HasPrefix(line, "probe error on ") || strings.HasPrefix(line, "no table "):
			lines[i] = s.paint(s.theme.Error, line)
		case strings.HasPrefix(line, fieldsSampleIndent):
			lines[i] = s.fieldsSample(line)
		}
	}
	return strings.Join(lines, "\n")
}

// fieldsSample paints one field line of writeFieldsSamples: the name and,
// after " e.g. ", the sample values.
func (s *interactiveStyle) fieldsSample(line string) string {
	body := line[len(fieldsSampleIndent):]
	name, values, found := strings.Cut(body, fieldsSamplesSeparator)
	if !found {
		return fieldsSampleIndent + s.paint(s.theme.Key, body)
	}
	trimmed := strings.TrimRight(name, " ")
	return fieldsSampleIndent + s.paint(s.theme.Key, trimmed) + name[len(trimmed):] +
		fieldsSamplesSeparator + s.paint(s.theme.Value, values)
}

// candidateKind is the kind of a Tab completion candidate, which decides
// its style in the listing.
type candidateKind int

const (
	// candidateCommand is the default kind: control commands and flags.
	candidateCommand candidateKind = iota
	candidateKeyword
	candidateField
	candidateVariable
	candidateFunction
	candidateOperator
	candidateValue
	candidateTable
	candidatePath
	candidateDirectory
)

// kindStyle returns the theme style of kind.
func (s *interactiveStyle) kindStyle(kind candidateKind) config.TermStyle {
	theme := s.themeOf()
	return [...]config.TermStyle{
		candidateCommand:   theme.Command,
		candidateKeyword:   theme.CandidateKeyword,
		candidateField:     theme.CandidateField,
		candidateVariable:  theme.CandidateVariable,
		candidateFunction:  theme.CandidateFunction,
		candidateOperator:  theme.CandidateOperator,
		candidateValue:     theme.CandidateValue,
		candidateTable:     theme.CandidateTable,
		candidatePath:      theme.CandidatePath,
		candidateDirectory: theme.CandidateDirectory,
	}[kind]
}

// stripANSI removes the SGR sequences (ESC [ ... m) paint adds, e.g. to
// count the columns of a painted prompt.
func stripANSI(text string) string {
	var sb strings.Builder
	for {
		start := strings.Index(text, "\x1b[")
		if start < 0 {
			sb.WriteString(text)
			return sb.String()
		}
		end := strings.IndexByte(text[start:], 'm')
		if end < 0 {
			sb.WriteString(text)
			return sb.String()
		}
		sb.WriteString(text[:start])
		text = text[start+end+1:]
	}
}

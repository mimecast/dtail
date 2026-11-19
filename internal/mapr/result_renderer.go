package mapr

import "strings"

// ResultRenderer formats terminal table output for mapreduce results.
type ResultRenderer interface {
	WriteHeaderEntry(sb *strings.Builder, text string, isSortKey, isGroupKey bool)
	WriteHeaderDelimiter(sb *strings.Builder, text string)
	WriteDataEntry(sb *strings.Builder, text string)
	WriteDataDelimiter(sb *strings.Builder, text string)
}

type plainResultRenderer struct{}

// PlainResultRenderer returns a renderer that writes uncolored terminal output.
func PlainResultRenderer() ResultRenderer {
	return plainResultRenderer{}
}

func (plainResultRenderer) WriteHeaderEntry(sb *strings.Builder, text string, _, _ bool) {
	sb.WriteString(text)
}

func (plainResultRenderer) WriteHeaderDelimiter(sb *strings.Builder, text string) {
	sb.WriteString(text)
}

func (plainResultRenderer) WriteDataEntry(sb *strings.Builder, text string) {
	sb.WriteString(text)
}

func (plainResultRenderer) WriteDataDelimiter(sb *strings.Builder, text string) {
	sb.WriteString(text)
}

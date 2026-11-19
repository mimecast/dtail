package clients

// itemBlank is the blank Tab inserted after a finished query item at the
// end of a query (queryCompletion.lead, completion.itemBlank): the
// line as Tab left it, the cursor right after the blank and the blank's
// length in bytes (more than one where the open shell quote escapes it).
// A comma typed right there replaces the blank (count(x) , -> count(x),),
// as the listing Tab printed with the blank says that a comma may follow.
type itemBlank struct {
	line      string
	pos, size int
}

// rememberItemBlank remembers the item blank result inserted, or forgets
// the one remembered when result inserted none.
func (e *interactiveEditor) rememberItemBlank(result completion) {
	e.itemBlank = itemBlank{}
	if result.itemBlank > 0 && result.itemBlank <= result.pos {
		e.itemBlank = itemBlank{line: result.line, pos: result.pos, size: result.itemBlank}
	}
}

// commaAfterItemBlank handles a key that follows the item blank Tab
// inserted: a comma with the line and cursor as Tab left them replaces the
// blank; it reports false for any other key or line, which the Terminal
// then handles as usual. Every key callback forgets the item blank first
// (takeItemBlank), so only the very next key the callback sees can replace
// it; keys the Terminal handles without the callback (cursor moves,
// Backspace) are caught by the line and cursor check.
func (b itemBlank) commaAfterItemBlank(line string, pos int, key rune) (string, int, bool) {
	if key != ',' || b.size == 0 || line != b.line || pos != b.pos {
		return "", 0, false
	}
	start := pos - b.size
	return line[:start] + "," + line[pos:], start + 1, true
}

// takeItemBlank returns the remembered item blank and forgets it.
func (e *interactiveEditor) takeItemBlank() itemBlank {
	blank := e.itemBlank
	e.itemBlank = itemBlank{}
	return blank
}

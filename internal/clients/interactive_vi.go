package clients

import (
	"unicode"
	"unicode/utf8"
)

// viEscapeKey is the key the Terminal passes to the key callback for a lone
// Esc in the vi editing mode (editInput): x/term would otherwise keep a
// lone ESC as the start of an escape sequence and read it together with
// the next key as one unknown key. Like externalEditKey it is valid UTF-8,
// no key a keyboard sends, and outside x/term's private key range.
const viEscapeKey = utf8.MaxRune - 1

// viCtrlR is Ctrl-R (redo in normal mode); x/term does not bind it.
const viCtrlR = 0x12

// viUndoMax bounds the undo steps kept for a line.
const viUndoMax = 100

type viMode int

const (
	viInsert viMode = iota
	viNormal
)

// viSnapshot is a line and cursor position to undo or redo to.
type viSnapshot struct {
	line []rune
	pos  int
}

// viHistory is the control line history, most recent first
// (controlHistory).
type viHistory interface {
	Len() int
	At(idx int) string
}

// viAliases are the normal mode keys that stand for an operator and a
// motion: x is dl, D is d$, S is cc and so on.
var viAliases = map[rune][2]rune{
	'x': {'d', 'l'}, 'X': {'d', 'h'}, 'D': {'d', '$'}, 'C': {'c', '$'},
	's': {'c', 'l'}, 'S': {'c', 'c'}, 'Y': {'y', 'y'},
}

// viEditor is the vi editing mode of the line editor (Client.EditMode
// "vi"), a layer over x/term's Terminal, which has no vi mode: every line
// starts in insert mode, where the Terminal edits as usual, and Esc
// switches to normal mode, where the key callback (editorKey) hands every
// key to key, which returns the changed line. Normal mode has the motions
// h l w b e W B E 0 ^ $ f F t T ; , (with counts), the operators d c y with
// a motion or doubled (dd cc yy), x X D C s S Y, i a I A, r ~ p P, u and
// Ctrl-R (undo, redo) and k j (history). Keys the Terminal handles itself
// never reach the callback and work in both modes as in insert mode:
// Enter, Backspace, the arrows, Home/End and the Ctrl keys.
type viEditor struct {
	mode viMode
	// count is the count typed so far; op and opCount an operator waiting
	// for its motion and the count typed before it.
	count, opCount int
	op             rune
	// pending is f, F, t, T or r waiting for its character, pendingCount
	// its count.
	pending      rune
	pendingCount int
	// lastFind and lastFindChar are the latest f/F/t/T, for ; and ,.
	lastFind, lastFindChar rune
	// repeating marks the motion of ; and , (motion: t/T skip the
	// character next to the cursor).
	repeating  bool
	register   []rune
	undo, redo []viSnapshot
	// insertStart is the line as insert mode started, an undo step when
	// insert mode changed it.
	insertStart viSnapshot
	history     viHistory
	// histIndex is the history entry k/j recalled (-1: the line being
	// edited, kept in histSaved meanwhile).
	histIndex int
	histSaved []rune
	// onMode is told every mode change (and the insert mode of every new
	// line), to show the mode with the cursor shape.
	onMode func(viMode)
}

func newViEditor(history viHistory, onMode func(viMode)) *viEditor {
	v := &viEditor{history: history, onMode: onMode}
	v.reset()
	return v
}

// reset starts a new line: insert mode, no undo steps, nothing pending.
// The register is kept, as in vi.
func (v *viEditor) reset() {
	*v = viEditor{register: v.register, history: v.history, onMode: v.onMode, histIndex: -1}
	v.setMode(viInsert)
}

func (v *viEditor) setMode(mode viMode) {
	v.mode = mode
	if v.onMode != nil {
		v.onMode(mode)
	}
}

// key handles key on line at byte position pos and returns the changed
// line and position and true, or false for a key the Terminal is to handle
// (every key in insert mode but Esc; Tab, which completes, in both).
func (v *viEditor) key(line string, pos int, key rune) (string, int, bool) {
	if key == '\t' {
		// Tab completes in both modes; it ends a count or operator typed
		// before it.
		v.cancel()
		return "", 0, false
	}
	if v.mode == viInsert && key != viEscapeKey {
		return "", 0, false
	}
	runes := []rune(line)
	rpos := utf8.RuneCountInString(line[:pos])
	if v.mode == viInsert {
		v.leaveInsert(runes)
		rpos--
	} else {
		runes, rpos = v.normalKey(runes, rpos, key)
	}
	if v.mode == viNormal {
		rpos = viClamp(runes, rpos)
	}
	return string(runes), len(string(runes[:rpos])), true
}

// viClamp keeps pos on a character, as normal mode has no position past
// the line's end.
func viClamp(line []rune, pos int) int {
	return max(min(pos, len(line)-1), 0)
}

func (v *viEditor) enterInsert(before viSnapshot) {
	v.cancel()
	v.insertStart = before
	v.setMode(viInsert)
}

// leaveInsert returns to normal mode; the insert is one undo step.
func (v *viEditor) leaveInsert(line []rune) {
	if string(line) != string(v.insertStart.line) {
		v.pushUndo(v.insertStart)
	}
	v.setMode(viNormal)
}

func (v *viEditor) pushUndo(s viSnapshot) {
	v.undo = append(v.undo, s)
	if len(v.undo) > viUndoMax {
		v.undo = v.undo[1:]
	}
	v.redo = nil
}

// cancel drops a count, operator or character wait in progress.
func (v *viEditor) cancel() {
	v.count, v.op, v.opCount, v.pending = 0, 0, 0, 0
}

func snapshot(line []rune, pos int) viSnapshot {
	return viSnapshot{line: append([]rune(nil), line...), pos: pos}
}

// normalKey handles a key in normal mode.
func (v *viEditor) normalKey(line []rune, pos int, key rune) ([]rune, int) {
	// The Terminal moves the cursor itself for the keys it handles (Up,
	// Down, End, Right), also past the line's end in normal mode.
	pos = viClamp(line, pos)
	if v.pending != 0 {
		return v.pendingKey(line, pos, key)
	}
	if key >= '1' && key <= '9' || key == '0' && v.count > 0 {
		v.count = min(v.count*10+int(key-'0'), 9999)
		return line, pos
	}
	count := max(v.count, 1)
	v.count = 0
	if alias, ok := viAliases[key]; ok && v.op == 0 {
		v.op, v.opCount = alias[0], count
		if alias[1] == alias[0] {
			return v.operator(line, pos, alias[1], 1)
		}
		return v.motion(line, pos, alias[1], 0, 1)
	}
	switch key {
	case 'd', 'c', 'y':
		return v.operator(line, pos, key, count)
	case 'f', 'F', 't', 'T', 'r':
		if key == 'r' && v.op != 0 {
			v.cancel()
			return line, pos
		}
		v.pending, v.pendingCount = key, count
		return line, pos
	case ';', ',':
		return v.repeatFind(line, pos, key == ',', count)
	}
	if _, _, ok := viMotion(line, pos, key, 0, 1); ok {
		return v.motion(line, pos, key, 0, count)
	}
	if v.op != 0 {
		v.cancel()
		return line, pos
	}
	return v.command(line, pos, key, count)
}

// pendingKey takes the character of f, F, t, T or r.
func (v *viEditor) pendingKey(line []rune, pos int, char rune) ([]rune, int) {
	kind, count := v.pending, v.pendingCount
	v.pending = 0
	if char == viEscapeKey || !insertsText(char) {
		v.cancel()
		return line, pos
	}
	if kind == 'r' {
		return v.replace(line, pos, char, count)
	}
	v.lastFind, v.lastFindChar = kind, char
	return v.motion(line, pos, kind, char, count)
}

// repeatFind repeats the latest f/F/t/T (;), or the other way round (,).
func (v *viEditor) repeatFind(line []rune, pos int, reverse bool, count int) ([]rune, int) {
	kind := v.lastFind
	if kind == 0 {
		v.cancel()
		return line, pos
	}
	if reverse {
		kind = map[rune]rune{'f': 'F', 'F': 'f', 't': 'T', 'T': 't'}[kind]
	}
	v.repeating = true
	return v.motion(line, pos, kind, v.lastFindChar, count)
}

// operator starts operator op (d, c, y), or applies it to the whole line
// when it is typed twice (dd, cc, yy).
func (v *viEditor) operator(line []rune, pos int, op rune, count int) ([]rune, int) {
	if v.op == 0 {
		v.op, v.opCount = op, count
		return line, pos
	}
	if v.op != op {
		v.cancel()
		return line, pos
	}
	v.op, v.opCount = 0, 0
	changed, at := v.apply(op, line, pos, 0, len(line))
	if op == 'y' {
		at = pos
	}
	return changed, at
}

// motion moves the cursor with key, or applies the waiting operator to the
// text between the cursor and where key moves. cw changes to the end of
// the word, as in vi.
func (v *viEditor) motion(line []rune, pos int, key, char rune, count int) ([]rune, int) {
	// No motion goes further than the line is long: a larger count only
	// costs time (9999d9999e).
	op, total := v.op, min(count*max(v.opCount, 1), len(line)+1)
	v.op, v.opCount = 0, 0
	repeat := v.repeating
	v.repeating = false
	if op == 'c' && (key == 'w' || key == 'W') && pos < len(line) &&
		viCharClass(line[pos], false) != 0 {
		end := viChangeWordEnd(line, pos, key == 'W', total)
		return v.apply(op, line, pos, pos, end+1)
	}
	from := pos
	if repeat && (key == 't' && pos+1 < len(line) && line[pos+1] == char ||
		key == 'T' && pos > 0 && line[pos-1] == char) {
		// ; or , after t/T: search past the character the cursor stands
		// next to, which it found last time, as vim does.
		from += map[rune]int{'t': 1, 'T': -1}[key]
	}
	target, inclusive, ok := viMotion(line, from, key, char, total)
	if !ok {
		return line, pos
	}
	if op == 0 {
		return line, target
	}
	from, to := pos, target
	if target < pos {
		from, to = target, pos
	} else if inclusive {
		to = target + 1
	}
	return v.apply(op, line, pos, from, min(to, len(line)))
}

// viChangeWordEnd is where cw with count ends: the end of the word at pos
// (also when pos is its last character), then of count-1 words more.
func viChangeWordEnd(line []rune, pos int, big bool, count int) int {
	end := pos
	class := viCharClass(line[pos], big)
	for end+1 < len(line) && viCharClass(line[end+1], big) == class {
		end++
	}
	for i := 1; i < count; i++ {
		end = viWordEnd(line, end, big)
	}
	return end
}

// apply runs op on line[from:to]: the text goes to the register, d and c
// delete it, c then inserts in its place.
func (v *viEditor) apply(op rune, line []rune, pos, from, to int) ([]rune, int) {
	before := snapshot(line, pos)
	if from == to {
		// Nothing deleted or yanked (x or S on an empty line, X at the
		// start): the register keeps its text, as in vi.
		if op == 'c' {
			v.enterInsert(before)
		}
		return line, from
	}
	v.register = append([]rune(nil), line[from:to]...)
	if op == 'y' {
		return line, from
	}
	changed := append(append([]rune(nil), line[:from]...), line[to:]...)
	if op == 'c' {
		v.enterInsert(before)
	} else if from != to {
		v.pushUndo(before)
	}
	return changed, from
}

// command handles the normal mode keys that are neither motions nor
// operators.
func (v *viEditor) command(line []rune, pos int, key rune, count int) ([]rune, int) {
	switch key {
	case 'i':
		v.enterInsert(snapshot(line, pos))
	case 'a':
		v.enterInsert(snapshot(line, pos))
		pos = min(pos+1, len(line))
	case 'I':
		v.enterInsert(snapshot(line, pos))
		pos = viFirstNonBlank(line)
	case 'A':
		v.enterInsert(snapshot(line, pos))
		pos = len(line)
	case '~':
		return v.toggleCase(line, pos, count)
	case 'p', 'P':
		return v.put(line, pos, key == 'p', count)
	case 'u':
		return v.undoStep(line, pos, &v.undo, &v.redo)
	case viCtrlR:
		return v.undoStep(line, pos, &v.redo, &v.undo)
	case 'k', 'j':
		return v.recall(line, pos, key == 'k', count)
	}
	return line, pos
}

// replace replaces count characters from pos with char (r).
func (v *viEditor) replace(line []rune, pos int, char rune, count int) ([]rune, int) {
	if pos+count > len(line) {
		return line, pos
	}
	v.pushUndo(snapshot(line, pos))
	changed := append([]rune(nil), line...)
	for i := pos; i < pos+count; i++ {
		changed[i] = char
	}
	return changed, pos + count - 1
}

// toggleCase switches the case of count characters from pos (~).
func (v *viEditor) toggleCase(line []rune, pos int, count int) ([]rune, int) {
	if len(line) == 0 {
		return line, pos
	}
	v.pushUndo(snapshot(line, pos))
	changed := append([]rune(nil), line...)
	end := min(pos+count, len(changed))
	for i := pos; i < end; i++ {
		if unicode.IsUpper(changed[i]) {
			changed[i] = unicode.ToLower(changed[i])
		} else {
			changed[i] = unicode.ToUpper(changed[i])
		}
	}
	return changed, end
}

// put inserts the register count times after (p) or before (P) the
// cursor, which ends on its last character.
func (v *viEditor) put(line []rune, pos int, after bool, count int) ([]rune, int) {
	if len(v.register) == 0 {
		return line, pos
	}
	// The line editor takes fewer than editorMaxLine runes: a count beyond
	// that puts as many copies as fit (a huge one would hang the redraw).
	count = min(count, (editorMaxLine-1-len(line))/len(v.register))
	if count < 1 {
		return line, pos
	}
	v.pushUndo(snapshot(line, pos))
	at := pos
	if after && len(line) > 0 {
		at = pos + 1
	}
	var text []rune
	for i := 0; i < count; i++ {
		text = append(text, v.register...)
	}
	changed := append(append(append([]rune(nil), line[:at]...), text...), line[at:]...)
	return changed, at + len(text) - 1
}

// undoStep goes back to the latest snapshot of from (u: undo, Ctrl-R:
// redo) and keeps the current line in to.
func (v *viEditor) undoStep(line []rune, pos int, from, to *[]viSnapshot) ([]rune, int) {
	if len(*from) == 0 {
		return line, pos
	}
	last := (*from)[len(*from)-1]
	*from = (*from)[:len(*from)-1]
	*to = append(*to, snapshot(line, pos))
	return last.line, last.pos
}

// recall puts an older (k) or newer (j) history entry on the line, the
// cursor at its start; j past the newest entry gives back the line being
// edited. The undo steps belong to the line left behind and are dropped.
func (v *viEditor) recall(line []rune, pos int, older bool, count int) ([]rune, int) {
	if v.history == nil {
		return line, pos
	}
	index := v.histIndex
	if index == -1 {
		// A line recalled with Up/Down (the Terminal's own history index,
		// which k/j do not see) counts as that entry: k goes on from it.
		for i := 0; i < v.history.Len(); i++ {
			if v.history.At(i) == string(line) {
				// j past the newest entry gives this line back, also
				// one typed that equals an entry.
				v.histSaved = append([]rune(nil), line...)
				index = i
				v.histIndex = i
				break
			}
		}
	}
	for i := 0; i < count; i++ {
		if older && index+1 < v.history.Len() {
			index++
		} else if !older && index >= 0 {
			index--
		}
	}
	if index == v.histIndex {
		return line, pos
	}
	if v.histIndex == -1 {
		v.histSaved = append([]rune(nil), line...)
	}
	v.histIndex = index
	v.undo, v.redo = nil, nil
	if index == -1 {
		return v.histSaved, 0
	}
	return []rune(v.history.At(index)), 0
}

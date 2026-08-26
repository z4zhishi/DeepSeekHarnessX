package tui

import "strings"

// Editor is the raw-mode input line: a multi-line buffer with cursor, word
// motions, history recall and slash-command completion. It is a pure state
// machine — no terminal I/O — so every behavior below is unit-testable; the
// screen layer only renders View() output and feeds HandleKey.
//
// Key contract (documented in /help and README):
//
//	Enter            submit (accept highlighted completion when popup open)
//	Ctrl+J/Alt+Enter insert newline (multi-line input)
//	↑/↓              completion selection when popup open; otherwise visual
//	                 line movement inside multi-line buffers, then history
//	Ctrl+P/Ctrl+N    history always (unambiguous alternative to ↑/↓)
//	Tab/Shift+Tab    accept completion / reverse-cycle candidates
//	Esc              close popup; else flag interrupt (abort turn / clear)
//	Alt+B/F, Alt+BS  word back / forward / delete-word-before
//	Ctrl+A/E/K/U/W   line start/end, kill-to-end/start, delete-word
type Editor struct {
	buf        []rune
	cur        int // cursor position as rune index into buf [0..len(buf)]
	width      int // content viewport width in cells (>0)
	prompt     string
	contPrompt string

	hist *History
	defs []commandInfo

	popupOpen bool
	matches   []commandInfo
	sel       int
}

func newEditor(defs []commandInfo, hist *History) *Editor {
	return &Editor{
		width:      80,
		prompt:     promptStr,
		contPrompt: ColorGray + "· " + ColorReset,
		hist:       hist,
		defs:       defs,
	}
}

// editorAction reports what the caller should do after one key.
type editorAction struct {
	Submit    string // non-empty: submit this text (already added to history)
	Interrupt bool   // bare Esc not consumed by the completion popup
}

// SetWidth updates the viewport width (re-queried per redraw for resizes).
func (e *Editor) SetWidth(w int) {
	if w > 0 {
		e.width = w
	}
}

// Buffer returns the current text.
func (e *Editor) Buffer() string { return string(e.buf) }

// Empty reports whether the buffer holds no runes.
func (e *Editor) Empty() bool { return len(e.buf) == 0 }

// Clear empties the buffer and closes the popup.
func (e *Editor) Clear() {
	e.buf = e.buf[:0]
	e.cur = 0
	e.closePopup()
}

// SetPrompt swaps the first-row prefix (approval mode uses "? ").
func (e *Editor) SetPrompt(p string) { e.prompt = p }

// PopupVisible reports whether the completion popup is showing.
func (e *Editor) PopupVisible() bool { return e.popupOpen }

// ForceCompletion opens the slash-command popup for the current buffer even
// when the cursor is not mid-token (used by Ctrl+P command palette). If the
// buffer is empty it assumes a leading "/" so the palette lists all commands;
// the user can then type to narrow. Safe to call when a popup already shows.
func (e *Editor) ForceCompletion() {
	if strings.ContainsAny(e.Buffer(), " ") {
		return // composing prose; don't force a command list over it
	}
	if !strings.HasPrefix(e.Buffer(), "/") {
		e.buf = append([]rune{'/'}, e.buf...)
		e.cur++
	}
	e.refreshCompletion()
}

// ---------------------------------------------------------------- key entry

func (e *Editor) HandleKey(k keyEvent) editorAction {
	switch k.kind {
	case keyRune:
		e.insert(k.r)
	case keyPaste:
		for _, r := range k.paste {
			e.insert(r)
		}
	case keyCtrlJ, keyAltEnter:
		e.insert('\n')
	case keyBackspace:
		if e.cur > 0 {
			e.buf = append(e.buf[:e.cur-1], e.buf[e.cur:]...)
			e.cur--
		}
	case keyAltBS:
		e.deleteWordBefore()
	case keyDelete:
		if e.cur < len(e.buf) {
			e.buf = append(e.buf[:e.cur], e.buf[e.cur+1:]...)
		}
	case keyLeft:
		if e.cur > 0 {
			e.cur--
		}
	case keyRight:
		if e.cur < len(e.buf) {
			e.cur++
		}
	case keyHome:
		e.lineStart()
	case keyEnd:
		e.lineEnd()
	case keyAltRune:
		switch k.r {
		case 'b':
			e.wordBack()
		case 'f':
			e.wordForward()
		default:
			e.insert(k.r) // unclaimed Alt combos degrade to plain text
		}
	case keyUp:
		return e.upKey()
	case keyDown:
		return e.downKey()
	case keyCtrl:
		switch k.ctrl {
		case 'a':
			e.lineStart()
		case 'e':
			e.lineEnd()
		case 'k':
			e.killToEnd()
		case 'u':
			e.killToStart()
		case 'w':
			e.deleteWordBefore()
		case 'd':
			if e.cur < len(e.buf) {
				e.buf = append(e.buf[:e.cur], e.buf[e.cur+1:]...)
			}
		case 'p':
			return e.historyPrev()
		case 'n':
			return e.historyNext()
		}
	case keyTab:
		if e.popupOpen && len(e.matches) > 0 {
			e.acceptSelected()
		}
	case keyShiftTab:
		if e.popupOpen && len(e.matches) > 0 {
			e.sel--
			if e.sel < 0 {
				e.sel = len(e.matches) - 1
			}
		}
	case keyEsc:
		if e.popupOpen {
			e.closePopup()
			return editorAction{}
		}
		return editorAction{Interrupt: true}
	case keyEnter:
		if e.popupOpen && len(e.matches) > 0 {
			e.acceptSelected()
			return editorAction{}
		}
		text := e.Buffer()
		e.Clear()
		if e.hist != nil {
			e.hist.Add(text)
		}
		return editorAction{Submit: text}
	}
	e.refreshCompletion()
	return editorAction{}
}

func (e *Editor) insert(r rune) {
	e.buf = append(e.buf, 0)
	copy(e.buf[e.cur+1:], e.buf[e.cur:])
	e.buf[e.cur] = r
	e.cur++
}

// ------------------------------------------------------------ line editing

// logicalLineBounds returns [start,end) rune indices of the logical line the
// cursor index sits on.
func logicalLineBounds(buf []rune, pos int) (start, end int) {
	start = pos
	for start > 0 && buf[start-1] != '\n' {
		start--
	}
	end = pos
	for end < len(buf) && buf[end] != '\n' {
		end++
	}
	return start, end
}

func (e *Editor) lineStart() {
	s, _ := logicalLineBounds(e.buf, e.cur)
	e.cur = s
}

func (e *Editor) lineEnd() {
	_, t := logicalLineBounds(e.buf, e.cur)
	e.cur = t
}

func (e *Editor) killToEnd() {
	_, t := logicalLineBounds(e.buf, e.cur)
	e.buf = append(e.buf[:e.cur], e.buf[t:]...)
	if e.cur > len(e.buf) {
		e.cur = len(e.buf)
	}
}

func (e *Editor) killToStart() {
	s, _ := logicalLineBounds(e.buf, e.cur)
	e.buf = append(e.buf[:s], e.buf[e.cur:]...)
	e.cur = s
}

func isWordRune(r rune) bool { return r != ' ' && r != '\t' && r != '\n' }

func (e *Editor) wordBack() {
	i := e.cur
	for i > 0 && !isWordRune(e.buf[i-1]) {
		i--
	}
	for i > 0 && isWordRune(e.buf[i-1]) {
		i--
	}
	e.cur = i
}

func (e *Editor) wordForward() {
	i := e.cur
	for i < len(e.buf) && !isWordRune(e.buf[i]) {
		i++
	}
	for i < len(e.buf) && isWordRune(e.buf[i]) {
		i++
	}
	e.cur = i
}

func (e *Editor) deleteWordBefore() {
	i := e.cur
	for i > 0 && !isWordRune(e.buf[i-1]) {
		i--
	}
	for i > 0 && isWordRune(e.buf[i-1]) {
		i--
	}
	e.buf = append(e.buf[:i], e.buf[e.cur:]...)
	e.cur = i
}

// ---------------------------------------------------------------- history

func (e *Editor) upKey() editorAction {
	if e.popupOpen {
		if e.sel > 0 {
			e.sel--
		}
		return editorAction{}
	}
	rows := e.layout()
	cr, _ := e.cursorVisual(rows)
	if cr > 0 {
		e.moveVisualRow(rows, cr, -1)
		return editorAction{}
	}
	return e.historyPrev()
}

func (e *Editor) downKey() editorAction {
	if e.popupOpen {
		if e.sel < len(e.matches)-1 {
			e.sel++
		}
		return editorAction{}
	}
	rows := e.layout()
	cr, _ := e.cursorVisual(rows)
	if cr < len(rows)-1 {
		e.moveVisualRow(rows, cr, +1)
		return editorAction{}
	}
	return e.historyNext()
}

func (e *Editor) historyPrev() editorAction {
	if e.hist == nil {
		return editorAction{}
	}
	entry, ok := e.hist.Prev(e.Buffer())
	if !ok {
		return editorAction{}
	}
	e.SetBuffer(entry)
	return editorAction{}
}

func (e *Editor) historyNext() editorAction {
	if e.hist == nil {
		return editorAction{}
	}
	entry, _, ok := e.hist.Next()
	if !ok {
		return editorAction{}
	}
	e.SetBuffer(entry)
	return editorAction{}
}

// SetBuffer replaces the buffer content, cursor at the end (used to restore
// the pre-approval draft and for history recall).
func (e *Editor) SetBuffer(s string) {
	e.buf = []rune(s)
	e.cur = len(e.buf)
	e.refreshCompletion()
}

// -------------------------------------------------------------- completion

// refreshCompletion recomputes the popup state from buffer + cursor. Called
// after every mutation.
func (e *Editor) refreshCompletion() {
	token, ok := completionToken(e.Buffer())
	if !ok || len(e.defs) == 0 {
		e.closePopup()
		return
	}
	matches := filterCommands(token, e.defs)
	if len(matches) == 0 {
		e.closePopup()
		return
	}
	if !e.popupOpen {
		e.popupOpen = true
		e.sel = 0
	}
	e.matches = matches
	if e.sel >= len(matches) {
		e.sel = len(matches) - 1
	}
}

func (e *Editor) closePopup() {
	e.popupOpen = false
	e.matches = nil
	e.sel = 0
}

// acceptSelected replaces "/tok" with "/name " (token always starts at index
// 1 because completionToken guarantees no spaces before the cursor).
func (e *Editor) acceptSelected() {
	if len(e.matches) == 0 {
		return
	}
	name := e.matches[e.sel].Name
	text := acceptCompletion("", name)
	e.buf = []rune(text)
	e.cur = len(e.buf)
	e.closePopup()
}

// PopupView renders the popup rows, or nil when closed.
func (e *Editor) PopupView() []string {
	if !e.popupOpen || len(e.matches) == 0 {
		return nil
	}
	return strings.Split(renderCompletionPopup(e.matches, e.sel, e.width), "\n")
}

// ---------------------------------------------------------------- layout

type vrow struct {
	rs, re  int // half-open range of rune indices displayed on this row
	prefixW int
}

// layout wraps the buffer into visual rows for the current width. Row 0 of
// each logical line carries its prompt prefix; continuation rows are plain.
func (e *Editor) layout() []vrow {
	var rows []vrow
	lineStart := 0
	logicalIdx := 0
	for lineStart <= len(e.buf) {
		lineEnd := lineStart
		for lineEnd < len(e.buf) && e.buf[lineEnd] != '\n' {
			lineEnd++
		}
		prefix := e.contPrompt
		if logicalIdx == 0 {
			prefix = e.prompt
		}
		pw := stringWidth(prefix)
		avail := e.width - pw
		if avail < 4 {
			avail = 4 // degenerate narrow terminals still make progress
		}
		rs := lineStart
		w := 0
		for i := lineStart; i < lineEnd; i++ {
			rw := runeWidth(e.buf[i])
			if w+rw > avail && i > rs {
				rows = append(rows, vrow{rs: rs, re: i, prefixW: pw})
				rs = i
				w = 0
				pw = 0
				avail = e.width
			}
			w += rw
		}
		rows = append(rows, vrow{rs: rs, re: lineEnd, prefixW: pw})
		if lineEnd == len(e.buf) {
			break
		}
		lineStart = lineEnd + 1
		logicalIdx++
	}
	return rows
}

// cursorVisual locates the cursor within laid-out rows: (rowIndex, colCells).
func (e *Editor) cursorVisual(rows []vrow) (int, int) {
	pos := e.cur
	for idx, row := range rows {
		if pos >= row.rs && pos <= row.re {
			if pos == row.re && idx+1 < len(rows) && rows[idx+1].rs == pos &&
				rows[idx+1].prefixW == 0 {
				continue // wrap boundary: show at start of next wrapped row
			}
			col := row.prefixW
			for i := row.rs; i < pos; i++ {
				col += runeWidth(e.buf[i])
			}
			return idx, col
		}
	}
	last := rows[len(rows)-1]
	col := last.prefixW
	for i := last.rs; i < pos && i < last.re; i++ {
		col += runeWidth(e.buf[i])
	}
	return len(rows) - 1, col
}

// moveVisualRow shifts the cursor one visual row up/down, landing near the
// same content column (prompt widths excluded).
func (e *Editor) moveVisualRow(rows []vrow, curRow, delta int) {
	target := curRow + delta
	if target < 0 || target >= len(rows) {
		return
	}
	cur := rows[curRow]
	row := rows[target]
	contentCol := e.cursorColInRow(cur) - cur.prefixW
	if contentCol < 0 {
		contentCol = 0
	}
	pos := row.rs
	w := 0
	for i := row.rs; i < row.re; i++ {
		rw := runeWidth(e.buf[i])
		if w+rw > contentCol {
			break
		}
		w += rw
		pos = i + 1
	}
	e.cur = pos
}

func (e *Editor) cursorColInRow(row vrow) int {
	col := row.prefixW
	for i := row.rs; i < e.cur && i < row.re; i++ {
		col += runeWidth(e.buf[i])
	}
	return col
}

// View renders the visible rows (with prompts) plus the cursor's visual
// position. This is everything the screen layer needs to draw the input area.
func (e *Editor) View() (rows []string, crow, ccol int) {
	layout := e.layout()
	crow, ccol = e.cursorVisual(layout)
	rows = make([]string, len(layout))
	for i := range layout {
		var prefix string
		switch {
		case i == 0:
			prefix = e.prompt
		case layout[i-1].re < layout[i].rs: // hard newline between the rows
			prefix = e.contPrompt
		}
		rows[i] = prefix + string(e.buf[layout[i].rs:layout[i].re])
	}
	return rows, crow, ccol
}

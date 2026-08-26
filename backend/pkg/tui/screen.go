package tui

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// Screen owns the terminal's bottom region: [pending stream tail] /
// [completion popup] / [status bar] / [input rows]. Scrollback writes always
// erase this region first, print complete lines, then redraw it — so stream
// output flows naturally above a permanently visible prompt without ever
// interleaving with it (the failure mode of the old cooked-mode TUI).
//
// Invariant making erase/redraw sound: the region's FIRST row is either empty
// (previous scrollback ended with \n) or exactly the unterminated stream
// tail; erasing upward from the current cursor lands precisely where the
// next stream bytes belong.
type Screen struct {
	ch       chan screenFrame
	done     chan struct{}
	wg       sync.WaitGroup
	out      io.Writer
	provider func() bottomSnapshot

	lastRows int
}

type screenFrameKind int

const (
	sfScroll screenFrameKind = iota // text: complete lines, ends with \n
	sfRefresh
	sfClear
)

type screenFrame struct {
	kind screenFrameKind
	text string
}

// bottomSnapshot is everything needed to paint one frame of the region. All
// rows must fit within the terminal width (callers pre-wrap).
type bottomSnapshot struct {
	tail   []string // pre-wrapped unterminated stream tail rows / browse rows
	popup  []string // completion popup rows (nil when closed)
	status string   // status bar row
	input  []string // editor rows (nil in browse mode)
	curRow int      // cursor position within input rows
	curCol int      // cursor display column within that input row
	sel    int      // row index of the selected entry in browse mode (-1 none)
}

func newScreen(out io.Writer, provider func() bottomSnapshot) *Screen {
	s := &Screen{
		ch:       make(chan screenFrame, 256),
		done:     make(chan struct{}),
		out:      out,
		provider: provider,
	}
	s.wg.Add(1)
	go s.loop()
	return s
}

func (s *Screen) loop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case f := <-s.ch:
			switch f.kind {
			case sfScroll:
				fmt.Fprint(s.out, s.eraseCmd())
				fmt.Fprint(s.out, f.text)
				s.drawBottom()
			case sfRefresh:
				fmt.Fprint(s.out, s.eraseCmd())
				s.drawBottom()
			case sfClear:
				fmt.Fprint(s.out, s.eraseCmd())
				fmt.Fprint(s.out, "\033[2J\033[H")
				s.drawBottom()
			}
		}
	}
}

// eraseCmd builds the escape sequence clearing the previously drawn region,
// leaving the cursor at column 0 of its first row.
func (s *Screen) eraseCmd() string {
	if s.lastRows <= 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\r\033[2K")
	for i := 1; i < s.lastRows; i++ {
		b.WriteString("\033[1A\r\033[2K")
	}
	return b.String()
}

// drawBottom paints the region from the latest snapshot and parks the cursor
// inside the input area. Cursor rendering is suppressed around the whole
// update to avoid flicker.
func (s *Screen) drawBottom() {
	snap := s.provider()
	var rows []string
	rows = append(rows, snap.tail...)
	rows = append(rows, snap.popup...)
	if snap.status != "" {
		rows = append(rows, snap.status)
	}
	if len(snap.input) == 0 {
		// Browse mode (scrollback focused) has no prompt row: paint the
		// window + status and park the cursor (hidden) without needing input.
		s.lastRows = len(rows)
		// Ensure the region is repainted even with no input row.
		fmt.Fprint(s.out, "\033[?25l"+strings.Join(rows, "\r\n")+"\r\033[?25h")
		return
	}
	inputAt := len(rows)
	rows = append(rows, snap.input...)

	var b strings.Builder
	b.WriteString("\033[?25l")
	b.WriteString(strings.Join(rows, "\r\n"))
	crowAbs := inputAt + snap.curRow
	if up := len(rows) - 1 - crowAbs; up > 0 {
		fmt.Fprintf(&b, "\r\033[%dA", up)
	} else {
		b.WriteString("\r")
	}
	if snap.curCol > 0 {
		fmt.Fprintf(&b, "\033[%dC", snap.curCol)
	}
	b.WriteString("\033[?25h")
	fmt.Fprint(s.out, b.String())
	s.lastRows = len(rows)
}

func (s *Screen) send(f screenFrame) {
	select {
	case s.ch <- f:
	case <-s.done:
	default:
		if f.kind == sfRefresh {
			return
		}
		select {
		case s.ch <- f:
		case <-s.done:
		}
	}
}

// Scroll writes complete lines (text must end with \n).
func (s *Screen) Scroll(text string) { s.send(screenFrame{kind: sfScroll, text: text}) }

// Refresh repaints the bottom region only.
func (s *Screen) Refresh() { s.send(screenFrame{kind: sfRefresh}) }

// ClearAll wipes scrollback plus the pending tail and repaints.
func (s *Screen) ClearAll() { s.send(screenFrame{kind: sfClear}) }

// Close stops the loop goroutine.
func (s *Screen) Close() {
	close(s.done)
	s.wg.Wait()
}

// scrollKind tags a retained scrollback entry so the transcript can be
// navigated structurally (select an entry, jump between turns, fold a block),
// not merely paged as a flat line stream. This is the difference between a
// printer and a browseable scrollback (GrokBuild parity).
type scrollKind int

const (
	scrollPlain     scrollKind = iota // ordinary stream line (assistant delta / status)
	scrollUser                        // a user prompt line
	scrollAssistant                   // an assistant turn (start of a response)
	scrollTool                        // a tool call / result
	scrollReasoning                   // a thinking block
	scrollTurnStart                   // "[Turn Start]" boundary marker
	scrollTurnEnd                     // "[Turn Completed]" boundary marker
)

// scrollEntry is one navigable item in the retained transcript. It owns the
// rendered lines (with ANSI runs) plus the kind used for selection/folding.
// folded hides all but the first line (Vim h/l / e / E fold commands).
type scrollEntry struct {
	kind   scrollKind
	lines  []string // pre-wrapped display rows
	folded bool
}

// focusTarget is where the keyboard lives: the prompt editor, the scrollback
// browse pane, or a blocking card. GrokBuild model: Tab/Space hand focus
// between scrollback and prompt; each card takes the keyboard while open.
type focusTarget int

const (
	focusPrompt focusTarget = iota
	focusScrollback
	focusCard
)

// presenter glues the shared state (editor, stream tail, status data) to the
// Screen. It is safe for concurrent use by the main loop, the event pump and
// the screen goroutine.
type presenter struct {
	mu       sync.Mutex // guards tail + editor access from any goroutine
	sendMu   sync.Mutex // serializes frame emission so scroll order matches text order
	scr      *Screen
	ed       *Editor
	tail     string
	width    int // last known terminal width
	height   int
	statusFn func() StatusData
	// scrollback is the retained transcript as structured entries; sindex is
	// the selected entry (when focusScrollback); sview is the first visible
	// row of the window. sindex==-1 and sview==0 mean the live tail (default,
	// auto-follow). Positive sview windows back without changing selection.
	scrollback []scrollEntry
	sindex     int
	sview      int
	// focus is where keyboard input is delivered.
	focus focusTarget
	// palette is the Ctrl+P command-palette state. When open, snapshot renders
	// the fuzzy action list instead of the completion popup; actions are a
	// fixed list supplied by the host (app.go wires them to slash commands,
	// model pick, session actions, extension tabs).
	paletteOpen  bool
	paletteItems []paletteItem
	paletteSel   int
	paletteQuery string
	// cardParked is set when a blocking approval/question card has parked the
	// keyboard in the scrollback (Esc). The shortcuts bar then names the card
	// and the Tab route back, so it is never a mystery how to return.
	cardParked bool
	// approval card render state: when non-nil the keyboard is on a blocking
	// approval card and snapshot renders the option list with the current
	// selection highlighted (self-explanatory Tab/↑↓ cycling).
	approvalOpen  bool
	approvalTitle string
	approvalItems []string // option display labels (already human-readable)
	approvalSel   int
	approvalHint  string // named answer keys, e.g. "y/n/a/c"
}

func newPresenter(out io.Writer, ed *Editor, statusFn func() StatusData) *presenter {
	p := &presenter{ed: ed, statusFn: statusFn}
	p.width, p.height = 80, 24
	p.scr = newScreen(out, p.snapshot)
	return p
}

// resize updates the cached terminal size (queried once per user keypress and
// per status change — cheap syscalls, no signal plumbing).
func (p *presenter) resize() {
	if w, h, ok := terminalSize(); ok {
		p.mu.Lock()
		p.width, p.height = w, h
		p.mu.Unlock()
	}
}

func (p *presenter) dims() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.width, p.height
}

// Write routes arbitrary stream text (defaults to a plain assistant delta):
// complete lines scroll immediately, an unterminated tail stays pinned above
// the prompt until completed.
func (p *presenter) Write(text string) {
	p.WriteKind(text, scrollPlain)
}

// WriteKind routes text with a structural kind so the retained scrollback is
// navigable (select/jump-turn/fold), not just a flat paged line stream.
func (p *presenter) WriteKind(text string, kind scrollKind) {
	if text == "" {
		return
	}
	p.mu.Lock()
	pend := p.tail + text
	var head string
	if i := strings.LastIndexByte(pend, '\n'); i >= 0 {
		head = pend[:i+1]
		p.tail = pend[i+1:]
	} else {
		p.tail = pend
	}
	// Append every complete line to scrollback as a kinded entry so PgUp can
	// page back and ↑↓ can select, Shift←→ can jump turns, h/l can fold.
	if head != "" {
		for _, ln := range strings.SplitAfter(head, "\n") {
			if ln != "" {
				ln = strings.TrimSuffix(ln, "\n")
				p.scrollback = append(p.scrollback, scrollEntry{
					kind:  kind,
					lines: []string{ln},
				})
			}
		}
		// Cap the retained history at a sane bound to bound memory.
		if len(p.scrollback) > 10000 {
			p.scrollback = p.scrollback[len(p.scrollback)-10000:]
		}
	}
	p.mu.Unlock()

	if head != "" {
		p.sendMu.Lock()
		p.scr.Scroll(head)
		p.sendMu.Unlock()
	} else {
		p.scr.Refresh()
	}
}

// ScrollPage pages the transcript back (n<0) or forward (n>0) by n screenfuls.
// Chaining past the bottom snaps back to the live tail (auto-follow). It
// preserves the current selection.
func (p *presenter) ScrollPage(n int) {
	p.mu.Lock()
	rows := p.scrollRowsLocked()
	p.mu.Unlock()
	if rows == 0 {
		return
	}
	page := 24
	if _, h := p.dims(); h > 4 {
		page = h - 2
	}
	p.mu.Lock()
	if n < 0 {
		p.sview += page
	} else {
		p.sview -= page
	}
	if p.sview >= rows {
		p.sview = 0 // back to live
	} else if p.sview < 0 {
		p.sview = 0
	}
	p.mu.Unlock()
	p.scr.Refresh()
}

// scrollRowsLocked returns the total display rows of the retained scrollback.
// Caller holds p.mu.
func (p *presenter) scrollRowsLocked() int {
	n := 0
	for _, e := range p.scrollback {
		n += len(e.lines)
	}
	return n
}

// Focus returns the current keyboard focus target.
func (p *presenter) Focus() focusTarget {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.focus
}

// ParkCard records whether a blocking card has parked the keyboard in the
// scrollback, so the shortcuts bar can name the Tab route back.
func (p *presenter) ParkCard(parked bool) {
	p.mu.Lock()
	p.cardParked = parked
	p.mu.Unlock()
	p.scr.Refresh()
}

// CardParked reports whether a blocking card currently parks the keyboard.
func (p *presenter) CardParked() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cardParked
}

// OpenApprovalCard starts rendering a blocking approval option card (the
// keyboard owns it). title is the prompt headline; items are the option
// display labels (already human-readable); hint names the direct answer keys.
func (p *presenter) OpenApprovalCard(title string, items []string, hint string) {
	p.mu.Lock()
	p.approvalOpen = true
	p.approvalTitle = title
	p.approvalItems = items
	p.approvalHint = hint
	p.approvalSel = 0
	p.mu.Unlock()
	p.scr.Refresh()
}

// ApprovalMove changes the highlighted option by delta (clamped). Returns the
// new selected index so the caller keeps a single source of truth.
func (p *presenter) ApprovalMove(delta int) int {
	p.mu.Lock()
	if len(p.approvalItems) > 0 {
		p.approvalSel += delta
		if p.approvalSel < 0 {
			p.approvalSel = 0
		}
		if p.approvalSel >= len(p.approvalItems) {
			p.approvalSel = len(p.approvalItems) - 1
		}
	}
	sel := p.approvalSel
	p.mu.Unlock()
	p.scr.Refresh()
	return sel
}

// CloseApprovalCard stops rendering the approval card.
func (p *presenter) CloseApprovalCard() {
	p.mu.Lock()
	p.approvalOpen = false
	p.approvalItems = nil
	p.approvalHint = ""
	p.mu.Unlock()
	p.scr.Refresh()
}

// renderApprovalCard renders the blocking approval option list with the
// current selection highlighted. Returns display rows (nil when closed).
func (p *presenter) renderApprovalCard(w int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.approvalOpen || len(p.approvalItems) == 0 {
		return nil
	}
	rows := []string{divider(w, "Permission Required", "")}
	if truncateWidthPlain(p.approvalTitle, w-2) != "" {
		rows = append(rows, ColorYellow+truncateWidthPlain(p.approvalTitle, w-2)+ColorReset)
	}
	for i, item := range p.approvalItems {
		label := item
		if i == p.approvalSel {
			label = ColorReverse + "▸ " + item + ColorReset
		} else {
			label = "  " + item
		}
		rows = append(rows, label)
	}
	if p.approvalHint != "" {
		rows = append(rows, ColorGray+p.approvalHint+ColorReset)
	}
	return rows
}

// SetFocus moves the keyboard between the prompt editor and the scrollback
// browse pane. GrokBuild parity: Tab/Space hand off; Esc is never a focus key.
func (p *presenter) SetFocus(f focusTarget) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if f != p.focus {
		p.focus = f
		p.sindex = -1
		if f == focusScrollback && len(p.scrollback) > 0 {
			// Select the last entry and snap the view to it.
			p.sindex = len(p.scrollback) - 1
			p.sview = 0
		}
	}
	p.scr.Refresh()
}

// SelectMove moves the scrollback selection by delta entries (GrokBuild ↑↓ /
// k j). Clamps to the transcript bounds. Returns true if the selection moved.
func (p *presenter) SelectMove(delta int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.focus != focusScrollback || len(p.scrollback) == 0 {
		return false
	}
	cur := p.sindex
	if cur < 0 {
		cur = len(p.scrollback) - 1
	}
	next := cur + delta
	if next < 0 {
		next = 0
	}
	if next >= len(p.scrollback) {
		next = len(p.scrollback) - 1
	}
	if next == p.sindex {
		return false
	}
	p.sindex = next
	p.scr.Refresh()
	return true
}

// JumpTurn moves the selection to the next (delta>0) or previous (delta<0)
// user turn boundary (GrokBuild Shift+←/→, H/L). Returns whether it moved.
func (p *presenter) JumpTurn(delta int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.focus != focusScrollback || len(p.scrollback) == 0 {
		return false
	}
	cur := p.sindex
	if cur < 0 {
		cur = len(p.scrollback) - 1
	}
	i := cur
	for {
		i += delta
		if i < 0 || i >= len(p.scrollback) {
			return false
		}
		if p.scrollback[i].kind == scrollTurnStart {
			p.sindex = i
			p.scr.Refresh()
			return true
		}
	}
}

// ToggleFold folds/unfolds the selected scrollback entry (Vim h/l, e). Only
// entries with more than one line are foldable; plain one-line rows are
// no-ops. Returns true when the selected entry's fold state changed.
func (p *presenter) ToggleFold() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.focus != focusScrollback || p.sindex < 0 || p.sindex >= len(p.scrollback) {
		return false
	}
	e := &p.scrollback[p.sindex]
	if len(e.lines) <= 1 {
		return false
	}
	e.folded = !e.folded
	p.scr.Refresh()
	return true
}

// ToggleAllFolds folds every multi-line entry, or unfolds all if any are
// folded (Vim Sh+E). Returns the number of entries whose state changed.
func (p *presenter) ToggleAllFolds() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	anyFolded := false
	for _, e := range p.scrollback {
		if e.folded {
			anyFolded = true
			break
		}
	}
	changed := 0
	for i := range p.scrollback {
		e := &p.scrollback[i]
		if len(e.lines) <= 1 {
			continue
		}
		want := !anyFolded // if none folded, fold all; else unfold all
		if e.folded != want {
			e.folded = want
			changed++
		}
	}
	if changed > 0 {
		p.scr.Refresh()
	}
	return changed
}

// RefreshInput repaints after an editor mutation.
func (p *presenter) RefreshInput() { p.scr.Refresh() }

// Clear resets scrollback and the pinned tail.
func (p *presenter) Clear() {
	p.mu.Lock()
	p.tail = ""
	p.mu.Unlock()
	p.sendMu.Lock()
	p.scr.ClearAll()
	p.sendMu.Unlock()
}

// FlushTail terminates a dangling partial stream line with a newline so the
// next region paint starts clean (used when a turn ends mid-delta).
func (p *presenter) FlushTail() {
	p.mu.Lock()
	dangling := p.tail != ""
	p.tail = ""
	p.mu.Unlock()
	if dangling {
		p.sendMu.Lock()
		p.scr.Scroll("\n")
		p.sendMu.Unlock()
	}
}

// Close tears down the screen loop.
func (p *presenter) Close() { p.scr.Close() }

// OpenPalette sets the action list and opens the Ctrl+P command palette.
func (p *presenter) OpenPalette(items []paletteItem) {
	p.mu.Lock()
	p.paletteItems = items
	p.paletteOpen = true
	p.paletteSel = 0
	p.paletteQuery = ""
	p.mu.Unlock()
	p.scr.Refresh()
}

// PaletteClose closes the command palette.
func (p *presenter) PaletteClose() {
	p.mu.Lock()
	p.paletteOpen = false
	p.paletteItems = nil
	p.paletteSel = 0
	p.paletteQuery = ""
	p.mu.Unlock()
	p.scr.Refresh()
}

// PaletteIsOpen reports whether the command palette is showing.
func (p *presenter) PaletteIsOpen() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paletteOpen
}

// PaletteType appends a character to the palette query (fuzzy filter).
func (p *presenter) PaletteType(r rune) {
	p.mu.Lock()
	p.paletteQuery += string(r)
	p.paletteSel = 0
	p.mu.Unlock()
	p.scr.Refresh()
}

// PaletteBackspace removes the last character of the palette query.
func (p *presenter) PaletteBackspace() {
	p.mu.Lock()
	if len(p.paletteQuery) > 0 {
		r := []rune(p.paletteQuery)
		p.paletteQuery = string(r[:len(r)-1])
		p.paletteSel = 0
	}
	p.mu.Unlock()
	p.scr.Refresh()
}

// PaletteMove changes the palette selection by delta (clamped to matches).
func (p *presenter) PaletteMove(delta int) {
	p.mu.Lock()
	matches := filterPalette(p.paletteItems, p.paletteQuery)
	if len(matches) == 0 {
		p.mu.Unlock()
		return
	}
	p.paletteSel += delta
	if p.paletteSel < 0 {
		p.paletteSel = 0
	}
	if p.paletteSel >= len(matches) {
		p.paletteSel = len(matches) - 1
	}
	p.mu.Unlock()
	p.scr.Refresh()
}

// PaletteRun executes the selected palette action and closes the palette.
// Returns false when there is nothing to run (palette closed or no match).
func (p *presenter) PaletteRun() bool {
	p.mu.Lock()
	matches := filterPalette(p.paletteItems, p.paletteQuery)
	var fn func()
	if p.paletteOpen && len(matches) > 0 && p.paletteSel >= 0 && p.paletteSel < len(matches) {
		fn = matches[p.paletteSel].Run
	}
	p.paletteOpen = false
	p.paletteItems = nil
	p.paletteSel = 0
	p.paletteQuery = ""
	p.mu.Unlock()
	if fn == nil {
		p.scr.Refresh()
		return false
	}
	p.scr.Refresh()
	fn()
	return true
}

// snapshot builds one paintable frame. Runs on the screen goroutine.
func (p *presenter) snapshot() bottomSnapshot {
	p.mu.Lock()
	w := p.width
	p.ed.SetWidth(w)
	tailRows := splitWrapANSI(p.tail, w)
	inRows, cr, cc := p.ed.View()
	popup := p.ed.PopupView()
	focus := p.focus
	sview := p.sview
	sindex := p.sindex
	palOpen := p.paletteOpen
	palItems := p.paletteItems
	palSel := p.paletteSel
	palQuery := p.paletteQuery
	cardParked := p.cardParked
	approvalOpen := p.approvalOpen
	p.mu.Unlock()

	// Blocking approval card: the keyboard owns it, so it renders instead of
	// the prompt editor (a blocking card is the highest-priority surface;
	// palette and prompt are never shown over it). Parked state defers to the
	// scrollback browse view so the user can read the context behind it.
	if approvalOpen && !cardParked {
		rows := p.renderApprovalCard(w)
		if len(rows) > 0 {
			return bottomSnapshot{
				tail:   rows,
				status: FormatStatusBar(p.statusFn(), w),
				input:  nil,
				curRow: 0,
				curCol: 0,
			}
		}
	}

	// Command palette (Ctrl+P) takes over the bottom region: the fuzzy action
	// list renders as its own pane (with a query row), and the prompt editor is
	// hidden so there is never a misleading second cursor.
	if palOpen {
		rows := renderPalette(palItems, palQuery, palSel, w)
		return bottomSnapshot{
			tail:   rows,
			status: FormatStatusBar(p.statusFn(), w),
			input:  nil,
			curRow: 0,
			curCol: 0,
		}
	}

	// Scrollback focus: render a browse window over the retained transcript
	// instead of the live tail, marking the selected entry. This is what makes
	// the transcript navigable (GrokBuild parity) rather than a paged printer.
	if focus == focusScrollback {
		body, sel := p.browseView(w, sview, sindex)
		if len(body) == 0 {
			body = []string{ColorDim + "(scrollback empty)" + ColorReset}
		}
		// While browsing, keep BOTH the live status bar (model/effort/cache/
		// busy — the operator must never lose those) and the contextual
		// shortcuts bar (GrokBuild parity: status is always-on, hints follow
		// the focused pane). The status row is appended as the last body row
		// so it reads under the transcript; the hint row is the status field.
		var bodyRows []string
		bodyRows = append(bodyRows, body...)
		bodyRows = append(bodyRows, FormatStatusBar(p.statusFn(), w))
		var hintItems []string
		if cardParked {
			// A blocking card owns the keyboard; the park route back is the
			// single most important hint and must never be trimmed away.
			hintItems = append(hintItems, shortcutHint("Tab", "card"))
			hintItems = append(hintItems, shortcutHint("Esc", "cancel"))
		} else {
			hintItems = append(hintItems, shortcutHint("Tab", "prompt"))
		}
		hintItems = append(hintItems,
			shortcutHint("↑↓", "select"),
			shortcutHint("Shift←→", "turn"),
			shortcutHint("PgUp/PgDn", "page"),
		)
		hints := byline(w, hintItems...)
		return bottomSnapshot{
			tail:   bodyRows,
			status: strings.TrimSuffix(hints, "\n"),
			input:  nil,
			// pin the cursor; browse mode has no prompt row
			curRow: 0,
			curCol: 0,
			sel:    sel,
		}
	}
	return bottomSnapshot{
		tail:   tailRows,
		popup:  popup,
		status: FormatStatusBar(p.statusFn(), w),
		input:  inRows,
		curRow: cr,
		curCol: cc,
	}
}

// browseView renders the scrollback window starting at sview (0 = oldest
// visible; the last entries fill the screen so the newest is always visible).
// It returns the display rows and the row index of the selected entry (or -1).
func (p *presenter) browseView(w, sview, sindex int) ([]string, int) {
	p.mu.Lock()
	entries := p.scrollback
	p.mu.Unlock()
	if len(entries) == 0 {
		return nil, -1
	}
	// Flatten entries into (row, entryIdx) display candidates.
	type row struct {
		text     string
		entryIdx int
	}
	var all []row
	for ei, e := range entries {
		display := e.lines
		if e.folded && len(display) > 1 {
			display = display[:1] // folded: show only the first line
		}
		for _, ln := range display {
			all = append(all, row{text: ln, entryIdx: ei})
		}
	}
	if len(all) == 0 {
		return nil, -1
	}
	// The window is the last N rows (sview counts back from the end). We cap
	// N to the available height below the status bar.
	_, h := p.dims()
	cap := h - 2
	if cap < 3 {
		cap = 3
	}
	start := len(all) - cap - sview
	if start < 0 {
		start = 0
	}
	window := all[start:]
	out := make([]string, 0, len(window))
	selRow := -1
	for _, r := range window {
		txt := r.text
		if r.entryIdx == sindex {
			txt = ColorReverse + "▸ " + txt + ColorReset
			selRow = len(out)
		}
		out = append(out, txt)
	}
	return out, selRow
}

// splitWrapANSI wraps s to display width, carrying the active SGR color
// across wrap boundaries (a colored stream delta must stay green on every
// wrapped row) and terminating each non-final row with a reset so color never
// bleeds into the following region row.
func splitWrapANSI(s string, width int) []string {
	if s == "" {
		return nil
	}
	if width < 1 {
		width = 1
	}
	runes := []rune(s)
	var rows []string
	var cur strings.Builder
	curW := 0
	lastSGR := ""

	flush := func(final bool) {
		row := cur.String()
		if !final && lastSGR != "" {
			// Terminate the colored row cleanly; the continuation row
			// re-opens the color itself so every row is self-contained.
			row += ColorReset
		}
		rows = append(rows, row)
		cur.Reset()
		curW = 0
		if !final && lastSGR != "" {
			cur.WriteString(lastSGR)
		}
	}

	i := 0
	for i < len(runes) {
		r := runes[i]
		if r == 0x1B {
			j := skipANSI(runes, i)
			seq := string(runes[i : j+1])
			cur.WriteString(seq)
			if isSGR(seq) {
				lastSGR = seq
			}
			i = j + 1
			continue
		}
		rw := runeWidth(r)
		if rw > 0 && curW+rw > width {
			flush(false)
		}
		cur.WriteRune(r)
		curW += rw
		i++
	}
	flush(true)
	return rows
}

// isSGR reports whether an escape sequence is a Select Graphic Rendition
// (color/attribute) command.
func isSGR(seq string) bool {
	runes := []rune(seq)
	if len(runes) < 3 || runes[0] != 0x1B || runes[1] != '[' {
		return false
	}
	final := runes[len(runes)-1]
	return final == 'm'
}

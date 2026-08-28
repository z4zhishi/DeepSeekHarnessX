package tui

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// keyKind enumerates the terminal key events the editor understands. The
// parser normalizes Windows VT input and xterm-style sequences into one set
// so editor logic stays platform-independent.
type keyKind int

const (
	keyRune       keyKind = iota // printable rune (k.r)
	keyEnter                     // CR — submit / accept completion
	keyCtrlJ                     // Ctrl+J (LF) — insert newline
	keyAltEnter                  // Alt+Enter — insert newline
	keyBackspace                 // BS / Ctrl+H / Alt+BS handled by editor via alt flag
	keyDelete                    // Del
	keyLeft                      // ←
	keyRight                     // →
	keyUp                        // ↑
	keyDown                      // ↓
	keyHome                      // Home
	keyEnd                       // End
	keyTab                       // Tab — accept completion
	keyShiftTab                  // BackTab — reverse cycle completion
	keyPageUp                    // PgUp — scrollback up
	keyPageDown                  // PgDn — scrollback down
	keyShiftLeft                 // Shift+Left — jump to previous turn
	keyShiftRight                // Shift+Right — jump to next turn
	keyMouse                     // mouse button / wheel event (k.mouse)
	keyEsc                       // lone ESC — popup close / interrupt
	keyCtrl                      // control char (k.ctrl holds 'a'..'z')
	keyAltRune                   // Alt+<printable> (k.r), e.g. Alt+B/F word jump
	keyAltBS                     // Alt+Backspace — delete word before cursor
	keyPaste                     // bracketed paste payload (k.paste)
	keyEOF                       // stdin closed
	keyUnknown                   // ignored noise
)

type keyEvent struct {
	kind  keyKind
	r     rune   // keyRune / keyAltRune
	ctrl  byte   // keyCtrl: lowercase letter ('c' = Ctrl+C)
	paste string // keyPaste
	mouse mouseEvent
}

// mouseEvent is one decoded terminal mouse action (SGR or X10 report). x/y
// are 1-based terminal coordinates; button 0-2 = left/middle/right, 4 = wheel
// up, 5 = wheel down (plus 32 for motion-drag). Pressed distinguishes press
// from release; the X10 encoding carries it in the code.
type mouseEvent struct {
	button  int
	x, y    int
	pressed bool
}

// escTimeout is how long the pump waits after a lone ESC lead byte before
// deciding it was a bare Escape rather than the head of an Alt-combo or CSI
// sequence. Short enough to keep interrupt latency imperceptible.
const escTimeout = 30 * time.Millisecond

// pasteFlushTimeout bounds a bracketed paste whose end marker never arrives
// (legacy terminals that ignore ?2004): the buffer flushes as one paste.
const pasteFlushTimeout = 200 * time.Millisecond

const (
	pasteStartSeq = "\x1b[200~"
	pasteEndSeq   = "\x1b[201~"
)

// keyParser is an incremental UTF-8 / VT-sequence decoder. Feed bytes as they
// arrive and collect emitted events. When Pending() reports true, schedule a
// Flush after pendingDeadline() so a bare ESC or an unterminated paste still
// resolves. All methods are single-goroutine synchronous — purity makes them
// unit-testable without a real terminal.
type keyParser struct {
	buf     []byte // unconsumed input bytes
	pending []byte // bytes of an unfinished escape/rune held for disambiguation
	paste   []rune // accumulated paste text while inside bracketed paste
	inPast  bool
	skipLF  bool // previous byte was CR; drop a following LF (Windows CRLF)
}

func newKeyParser() *keyParser { return &keyParser{} }

// Pending reports whether bytes are held awaiting disambiguation.
func (p *keyParser) Pending() bool {
	return len(p.pending) > 0 || p.inPast || p.hasPartialRune()
}

func (p *keyParser) pendingDeadline() time.Duration {
	if p.inPast {
		return pasteFlushTimeout
	}
	return escTimeout
}

// hasPartialRune reports whether buf starts with an incomplete-but-valid
// UTF-8 prefix.
func (p *keyParser) hasPartialRune() bool {
	if len(p.buf) == 0 || p.buf[0] < 0x80 || p.buf[0] >= 0xF8 {
		return false
	}
	return utf8SeqLen(p.buf[0]) > len(p.buf)
}

// Flush resolves held state: a dangling ESC becomes keyEsc (its continuation,
// if any, is re-fed); an open paste flushes as one event; a partial rune
// flushes lossily. Called by the pump on timeout.
func (p *keyParser) Flush() []keyEvent {
	var out []keyEvent
	if len(p.pending) > 0 {
		held := p.pending
		p.pending = nil
		if held[0] == 0x1B {
			out = append(out, keyEvent{kind: keyEsc})
			for _, b := range held[1:] {
				out = append(out, p.Feed(b)...)
			}
		} else {
			for _, b := range held {
				out = append(out, p.Feed(b)...)
			}
		}
		return out
	}
	if p.inPast {
		if len(p.buf) > 0 {
			appendBytesAsRunes(&p.paste, p.buf)
			p.buf = nil
		}
		ev := keyEvent{kind: keyPaste, paste: string(p.paste)}
		p.paste = nil
		p.inPast = false
		out = append(out, ev)
	}
	// An ordinary incomplete UTF-8 prefix has no recoverable meaning after
	// timeout; drop it so it cannot consume the next input byte.
	p.buf = nil
	return out
}

// Feed decodes input bytes, returning completed key events.
func (p *keyParser) Feed(b byte) []keyEvent {
	if len(p.pending) > 0 {
		// A sequence was held for disambiguation; new bytes continue it.
		p.buf = append(p.buf, p.pending...)
		p.pending = p.pending[:0]
	}
	p.buf = append(p.buf, b)
	return p.drain()
}

func (p *keyParser) drain() []keyEvent {
	var out []keyEvent
	for {
		if p.inPast {
			idx := indexPasteEnd(p.buf)
			if idx < 0 {
				// Retain only a suffix that could begin the end marker, or an
				// incomplete trailing UTF-8 sequence — flushing either early
				// would corrupt the paste (split marker or split rune).
				keep := 0
				max := len(p.buf)
				if max > len(pasteEndSeq)-1 {
					max = len(pasteEndSeq) - 1
				}
				for n := 1; n <= max; n++ {
					if strings.HasPrefix(pasteEndSeq, string(p.buf[len(p.buf)-n:])) {
						keep = n
					}
				}
				if u := utf8TailKeep(p.buf); u > keep {
					keep = u
				}
				appendBytesAsRunes(&p.paste, p.buf[:len(p.buf)-keep])
				p.buf = p.buf[len(p.buf)-keep:]
				return out
			}
			appendBytesAsRunes(&p.paste, p.buf[:idx])
			out = append(out, keyEvent{kind: keyPaste, paste: string(p.paste)})
			p.paste = nil
			p.inPast = false
			p.buf = p.buf[idx+len(pasteEndSeq):]
			continue
		}
		if len(p.buf) == 0 {
			return out
		}
		b := p.buf[0]
		if p.skipLF {
			p.skipLF = false
			if b == '\n' {
				p.buf = p.buf[1:]
				continue
			}
		}
		switch {
		case b == 0x1B:
			ev, consumed, more := p.parseEscape()
			if more {
				p.pending = append(p.pending[:0], p.buf...)
				p.buf = nil
				return out
			}
			if ev != nil {
				out = append(out, *ev)
			}
			p.buf = p.buf[consumed:]
		case b == '\r':
			out = append(out, keyEvent{kind: keyEnter})
			p.buf = p.buf[1:]
			p.skipLF = true
		case b == '\n':
			// LF arrives for Ctrl+J and for Enter on terminals configured to
			// translate CR; treating it as newline-insert (never submit) keeps
			// multi-line editing possible everywhere.
			out = append(out, keyEvent{kind: keyCtrlJ})
			p.buf = p.buf[1:]
		case b == '\t':
			out = append(out, keyEvent{kind: keyTab})
			p.buf = p.buf[1:]
		case b == 0x7F || b == 0x08:
			out = append(out, keyEvent{kind: keyBackspace})
			p.buf = p.buf[1:]
		case b < 0x20:
			out = append(out, keyEvent{kind: keyCtrl, ctrl: b + 'a' - 1})
			p.buf = p.buf[1:]
		default:
			need := utf8SeqLen(b)
			if need <= 0 {
				p.buf = p.buf[1:] // stray continuation byte: drop as noise
				continue
			}
			if len(p.buf) < need {
				return out // wait for the rest of the rune
			}
			r, ok := decodeExactUTF8(p.buf[:need])
			if ok && r > 0x1F && r != 0x7F {
				out = append(out, keyEvent{kind: keyRune, r: r})
			}
			p.buf = p.buf[need:]
		}
	}
}

// parseEscape classifies a sequence led by ESC using p.buf.
// Returns (event, bytesConsumed, needsMoreBytes).
func (p *keyParser) parseEscape() (*keyEvent, int, bool) {
	if len(p.buf) < 2 {
		return nil, 0, true
	}
	switch p.buf[1] {
	case '[': // CSI
		return p.parseCSI()
	case 'O': // SS3 application cursor mode: ESC O A …
		if len(p.buf) < 3 {
			return nil, 0, true
		}
		k := ss3Key(p.buf[2])
		return &keyEvent{kind: k}, 3, false
	case 0x1B:
		// Double ESC: deliver the first as a bare Escape immediately.
		return &keyEvent{kind: keyEsc}, 1, false
	case '\r', '\n':
		return &keyEvent{kind: keyAltEnter}, 2, false
	case 0x7F, 0x08:
		return &keyEvent{kind: keyAltBS}, 2, false
	default:
		c := p.buf[1]
		if c < 0x20 {
			return &keyEvent{kind: keyUnknown}, 2, false
		}
		need := utf8SeqLen(c)
		if need <= 1 {
			return &keyEvent{kind: keyAltRune, r: rune(c)}, 2, false
		}
		if len(p.buf) < 1+need {
			return nil, 0, true
		}
		r, ok := decodeExactUTF8(p.buf[1 : 1+need])
		if !ok {
			return &keyEvent{kind: keyUnknown}, 1 + need, false
		}
		return &keyEvent{kind: keyAltRune, r: r}, 1 + need, false
	}
}

// parseCSI handles ESC [ … sequences: arrows, Home/End, Delete, BackTab and
// the bracketed-paste start marker. A paste END seen outside a paste is noise.
func (p *keyParser) parseCSI() (*keyEvent, int, bool) {
	s := string(p.buf)
	if strings.HasPrefix(pasteStartSeq, s) && len(s) < len(pasteStartSeq) {
		return nil, 0, true
	}
	if strings.HasPrefix(s, pasteStartSeq) {
		p.inPast = true
		p.paste = nil
		return nil, len(pasteStartSeq), false
	}
	if strings.HasPrefix(pasteEndSeq, s) && len(s) < len(pasteEndSeq) {
		return nil, 0, true
	}
	if strings.HasPrefix(s, pasteEndSeq) {
		return nil, len(pasteEndSeq), false
	}
	// SGR mouse report: ESC [ < b ; x ; y M (press) / m (release). The '<'
	// marks it (plain CSI params never start with '<'); x/y are 1-based.
	// p.buf still holds the leading ESC, so '<' is at index 2.
	if len(s) >= 3 && s[2] == '<' {
		if ev, n, need := p.parseMouseCSI(); ev != nil || need {
			return ev, n, need
		}
	}
	// Scan parameters (0x30..0x3F) up to the final byte (@..~).
	i := 2
	for ; i < len(p.buf); i++ {
		c := p.buf[i]
		if c >= 0x40 && c <= 0x7E {
			ev := csiKey(string(p.buf[2:i]), c)
			return &ev, i + 1, false
		}
		if c < 0x30 || c > 0x3F {
			// Malformed CSI: swallow what we saw.
			return &keyEvent{kind: keyUnknown}, i + 1, false
		}
	}
	return nil, 0, true // incomplete CSI
}

// parseMouseCSI decodes an SGR mouse report `ESC [ < b ; x ; y M|m` out of
// p.buf. Returns (event, bytesConsumed, needsMoreBytes). A final byte that
// never arrives leaves need=true for the pending-flush disambiguation.
func (p *keyParser) parseMouseCSI() (*keyEvent, int, bool) {
	// p.buf = ESC [ < b ; x ; y final (ESC and '[' already present, '<' at 2).
	// Fields (b;x;y) begin at index 3, separated by ';'.
	if len(p.buf) < 3 || p.buf[0] != 0x1B || p.buf[1] != '[' || p.buf[2] != '<' {
		return nil, 0, false
	}
	i := 3
	for ; i < len(p.buf); i++ {
		c := p.buf[i]
		if c >= 0x40 && c <= 0x7E {
			break
		}
	}
	if i >= len(p.buf) {
		return nil, 0, true // no final byte yet
	}
	final := p.buf[i]
	if final != 'M' && final != 'm' {
		return nil, 0, false // not a mouse report after all
	}
	parts := strings.Split(string(p.buf[3:i]), ";")
	if len(parts) < 3 {
		return &keyEvent{kind: keyUnknown}, i + 1, false
	}
	bx, _ := strconv.Atoi(parts[0])
	px, _ := strconv.Atoi(parts[1])
	py, _ := strconv.Atoi(parts[2])
	button := bx & 0x03 // 0 left, 1 middle, 2 right
	ev := keyEvent{kind: keyMouse}
	switch {
	case bx&0x40 != 0 && bx&0x01 == 0:
		// Wheel up: code 64 (0x40) sets bit6, low bit clear.
		ev.mouse.button, ev.mouse.pressed = 4, true
	case bx&0x40 != 0 && bx&0x01 != 0:
		// Wheel down: code 65 sets bit6 + low bit.
		ev.mouse.button, ev.mouse.pressed = 5, true
	case bx&0x40 != 0:
		// Motion with a held button (drag): report the button only.
		ev.mouse.button = button
	default:
		ev.mouse.button = button
		ev.mouse.pressed = final == 'M'
	}
	ev.mouse.x, ev.mouse.y = px, py
	return &ev, i + 1, false
}

func csiKey(params string, final byte) keyEvent {
	// Shift-modified arrows arrive as ESC [ 1 ; 2 D / 1 ; 2 C (params="1;2").
	// These drive turn navigation (Shift+←/→) when the scrollback is focused.
	if params == "1;2" || params == "1;3" || params == "1;5" {
		switch final {
		case 'D':
			return keyEvent{kind: keyShiftLeft}
		case 'C':
			return keyEvent{kind: keyShiftRight}
		}
	}
	switch final {
	case 'A':
		return keyEvent{kind: keyUp}
	case 'B':
		return keyEvent{kind: keyDown}
	case 'C':
		return keyEvent{kind: keyRight}
	case 'D':
		return keyEvent{kind: keyLeft}
	case 'H':
		return keyEvent{kind: keyHome}
	case 'F':
		return keyEvent{kind: keyEnd}
	case 'Z':
		return keyEvent{kind: keyShiftTab}
	case '~':
		switch params {
		case "1", "7":
			return keyEvent{kind: keyHome}
		case "3":
			return keyEvent{kind: keyDelete}
		case "4", "8":
			return keyEvent{kind: keyEnd}
		case "5", "6":
			// Page Up / Page Down (also sent with modifiers e.g. 5;5~).
			if strings.HasPrefix(params, "5") {
				return keyEvent{kind: keyPageUp}
			}
			return keyEvent{kind: keyPageDown}
		case "200":
			// Reached only when markers were consumed above; defensive.
			return keyEvent{kind: keyUnknown}
		}
	}
	return keyEvent{kind: keyUnknown}
}

func ss3Key(c byte) keyKind {
	switch c {
	case 'A':
		return keyUp
	case 'B':
		return keyDown
	case 'C':
		return keyRight
	case 'D':
		return keyLeft
	case 'H':
		return keyHome
	case 'F':
		return keyEnd
	}
	return keyUnknown
}

// indexPasteEnd finds pasteEndSeq in b, or -1.
func indexPasteEnd(b []byte) int {
	return strings.Index(string(b), pasteEndSeq)
}

// utf8TailKeep returns the number of trailing bytes in b that form an
// incomplete UTF-8 sequence (a lead byte still waiting on continuations), so
// the paste flusher never splits one.
func utf8TailKeep(b []byte) int {
	n := 3
	if len(b) < n {
		n = len(b)
	}
	for k := n; k >= 1; k-- {
		lead := b[len(b)-k]
		if utf8SeqLen(lead) > k {
			return k
		}
	}
	return 0
}

// utf8SeqLen returns the encoded length implied by a UTF-8 lead byte, or -1
// for invalid lead/continuation bytes.
func utf8SeqLen(b byte) int {
	switch {
	case b < 0x80:
		return 1
	case b >= 0xC2 && b < 0xE0:
		return 2
	case b >= 0xE0 && b < 0xF0:
		return 3
	case b >= 0xF0 && b < 0xF5:
		return 4
	default:
		return -1
	}
}

func decodeExactUTF8(b []byte) (rune, bool) {
	if !utf8.Valid(b) {
		return 0, false
	}
	r, _ := utf8.DecodeRune(b)
	return r, true
}

func appendBytesAsRunes(dst *[]rune, b []byte) {
	for _, r := range string(b) {
		*dst = append(*dst, r)
	}
}

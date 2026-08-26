package tui

// Display-width helpers. The editor, completion popup and status bar all do
// their own wrapping and cursor math, so they need ANSI-aware East-Asian
// display widths instead of byte or rune counts (Chinese input is a primary
// use case for DSHX). This is a compact pragmatic wcwidth: it covers the
// blocks that matter for CJK terminals; exotic scripts fall back to width 1,
// which degrades gracefully rather than crashing.

type runeRange struct{ lo, hi rune }

// zeroWidth covers combining marks that commonly appear in terminal text
// (Latin/Greek/Cyrillic combining diacritics + the common Hebrew/Arabic
// ranges). Full Unicode coverage is out of scope on purpose.
var zeroWidth = []runeRange{
	{0x0300, 0x036F},                                     // combining diacritical marks
	{0x0483, 0x0489},                                     // combining Cyrillic
	{0x0591, 0x05BD},                                     // Hebrew pointing
	{0x0610, 0x061A},                                     // Arabic extension
	{0x064B, 0x065F},                                     // Arabic diacritics
	{0x0E31, 0x0E31}, {0x0E34, 0x0E3A}, {0x0E47, 0x0E4E}, // Thai
	{0x200B, 0x200F}, // zero-width space/marks
	{0xFE00, 0xFE0F}, // variation selectors
	{0xFE20, 0xFE2F}, // combining half marks
}

// wideWidth covers double-width blocks: CJK ideographs and friends, Hangul,
// Kana, fullwidth forms and the CJK punctuation block.
var wideWidth = []runeRange{
	{0x1100, 0x115F},   // Hangul Jamo leading consonants
	{0x2329, 0x232A},   // angle brackets
	{0x2E80, 0x303E},   // CJK radicals + Kangxi + CJK symbols/punctuation
	{0x3041, 0x33FF},   // Hiragana .. CJK compatibility
	{0x3400, 0x4DBF},   // CJK ext A
	{0x4E00, 0x9FFF},   // CJK unified
	{0xA000, 0xA4CF},   // Yi
	{0xAC00, 0xD7A3},   // Hangul syllables
	{0xF900, 0xFAFF},   // CJK compatibility ideographs
	{0xFE10, 0xFE19},   // vertical forms
	{0xFE30, 0xFE6F},   // CJK compatibility forms + small forms
	{0xFF00, 0xFF60},   // fullwidth forms
	{0xFFE0, 0xFFE6},   // fullwidth signs
	{0x17000, 0x187F7}, // Tangut
	{0x1F300, 0x1F64F}, // emoji pictographs (common in chat text)
	{0x1F900, 0x1F9FF}, // supplemental symbols
	{0x20000, 0x2FFFD}, // CJK ext B..
	{0x30000, 0x3FFFD}, // CJK ext G..
}

func inRanges(r rune, ranges []runeRange) bool {
	lo, hi := 0, len(ranges)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		switch {
		case r < ranges[mid].lo:
			hi = mid - 1
		case r > ranges[mid].hi:
			lo = mid + 1
		default:
			return true
		}
	}
	return false
}

// runeWidth returns the terminal cell count of r: 0 for combining marks,
// 2 for East-Asian wide runes, 1 otherwise. Control characters report 0 —
// they never appear inside editable buffers (the key parser consumes them).
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 32 || (r >= 0x7F && r < 0xA0):
		return 0
	case inRanges(r, zeroWidth):
		return 0
	case inRanges(r, wideWidth):
		return 2
	default:
		return 1
	}
}

// stringWidth measures the displayed cell width of s, skipping ANSI escape
// sequences (CSI and simple two-byte escapes). All bottom-region rows carry
// color codes, so every layout computation goes through this.
func stringWidth(s string) int {
	w := 0
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] == 0x1B {
			i = skipANSI(runes, i)
			continue
		}
		w += runeWidth(runes[i])
	}
	return w
}

// skipANSI returns the index of the last rune belonging to the escape
// sequence that starts at runes[i] (runes[i] == ESC). Handles CSI (\x1b[...
// final byte @ through ~) and the common two-byte escapes (\x1bX).
func skipANSI(runes []rune, i int) int {
	if i+1 >= len(runes) {
		return i // dangling ESC at end of buffer: treat as one cell-less rune
	}
	switch runes[i+1] {
	case '[': // CSI: consume until final byte in @..~
		for j := i + 2; j < len(runes); j++ {
			if runes[j] >= 0x40 && runes[j] <= 0x7E {
				return j
			}
		}
		return len(runes) - 1
	case ']', 'P', 'X', '^', '_': // OSC/DCS/SOS/PM/APC: run to BEL or ST
		for j := i + 2; j < len(runes); j++ {
			if runes[j] == 0x07 || (runes[j] == '\\' && runes[j-1] == 0x1B) {
				return j
			}
		}
		return len(runes) - 1
	default:
		return i + 1 // two-byte escape such as \x1b(B
	}
}

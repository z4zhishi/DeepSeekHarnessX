package tui

// comp.go — small reusable UI "components" (CCB @ant/ink design-system parity,
// translated to plain rune-width-aware ANSI rendering). Each returns a string
// that fits the terminal width; no terminal I/O, so everything is unit-testable
// and safe to call from the screen goroutine.
//
// These are the building blocks for the command palette, blocking cards, and
// the shortcuts bar introduced in Wave 2/3.

import (
	"strings"
)

// divider renders a horizontal rule, optionally with a centered title
// (CCB Divider). The line fills `width` columns with `char` (default '─').
func divider(width int, title, char string) string {
	if char == "" {
		char = "─"
	}
	line := strings.Repeat(char, width)
	if title == "" {
		return ColorGray + line + ColorReset + "\n"
	}
	title = " " + strings.TrimSpace(title) + " "
	tw := stringWidth(title)
	if tw >= width {
		return ColorGray + truncateWidthPlain(title, width) + ColorReset + "\n"
	}
	half := (width - tw) / 2
	left := strings.Repeat(char, half)
	right := strings.Repeat(char, width-tw-half)
	return ColorGray + left + ColorReset + title + ColorGray + right + ColorReset + "\n"
}

// byline renders a footer of middot-separated items (CCB Byline). Each item is
// usually a KeyboardShortcutHint rendered as "⌨ key action". Clamped to width.
func byline(width int, items ...string) string {
	if len(items) == 0 {
		return ""
	}
	joined := strings.Join(items, ColorGray + " · " + ColorReset)
	return truncateANSI(joined, width) + "\n"
}

// shortcutHint renders one "key → action" hint, e.g. "↑↓ navigate".
func shortcutHint(key, action string) string {
	return ColorBold + key + ColorReset + " " + action
}

// pane renders content inside a bordered box with a themed top-border color
// (CCB Pane). Each row is padded to `width`; the top border echoes `color`.
func pane(width int, color, title string, content []string) string {
	if width < 8 {
		width = 8
	}
	border := "─"
	if color == "" {
		color = ColorGray
	}
	top := color + strings.Repeat(border, width-2) + ColorReset
	if title != "" {
		inner := " " + truncateWidthPlain(title, width-6) + " "
		pad := width - 2 - stringWidth(top) // top is uncolored width
		_ = pad
		// Title overlaid at the left of the top border.
		t := strings.Repeat(border, 2) + color + inner + ColorReset + color + strings.Repeat(border, width-2-stringWidth(inner)-2) + ColorReset
		if stringWidth(color+strings.Repeat(border, width-2)+ColorReset) <= width {
			top = t
		}
	}
	var b strings.Builder
	b.WriteString(top)
	b.WriteString("\n")
	for _, row := range content {
		r := truncateANSI(row, width-2)
		b.WriteString(" " + r + strings.Repeat(" ", width-2-stringWidth(r)) + " \n")
	}
	b.WriteString(color + strings.Repeat(border, width-2) + ColorReset + "\n")
	return b.String()
}

// statusIcon renders a semantic status glyph+color (CCB StatusIcon).
func statusIcon(status string, withSpace bool) string {
	var glyph, color string
	switch status {
	case "success":
		glyph, color = "✔", ColorGreen
	case "error":
		glyph, color = "✘", ColorRed
	case "warning":
		glyph, color = "⚠", ColorYellow
	case "info":
		glyph, color = "ℹ", ColorBlue
	case "pending":
		glyph, color = "◌", ColorGray
	case "loading":
		glyph, color = "…", ColorDim
	default:
		glyph, color = "·", ColorGray
	}
	out := color + glyph + ColorReset
	if withSpace {
		out += " "
	}
	return out
}

// progressBar renders a horizontal progress bar (CCB ProgressBar).
func progressBar(ratio float64, width int, fillColor, emptyColor string) string {
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	if width < 2 {
		width = 2
	}
	if fillColor == "" {
		fillColor = ColorCyan
	}
	if emptyColor == "" {
		emptyColor = ColorGray
	}
	filled := int(ratio * float64(width))
	filledChar := "█"
	emptyChar := "░"
	return fillColor + strings.Repeat(filledChar, filled) + emptyColor + strings.Repeat(emptyChar, width-filled) + ColorReset
}

// loadingState renders a spinner-style loading row with an optional subtitle
// (CCB LoadingState). The spinner cycles by `tick`.
func loadingState(tick int, message, subtitle string, bold bool) string {
	spinners := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	spin := spinners[tick%len(spinners)]
	out := ColorCyan + spin + ColorReset + " "
	if bold {
		out += ColorBold + message + ColorReset
	} else {
		out += message
	}
	if subtitle != "" {
		out += ColorDim + " " + subtitle + ColorReset
	}
	return out
}

// listItem renders one selectable list row (CCB ListItem). When focused it is
// highlighted (selection marker); when disabled it is dimmed.
func listItem(text string, focused, selected, disabled bool, marker string) string {
	prefix := "  "
	if focused {
		prefix = ColorReverse + "▸ " + ColorReset
	} else if selected {
		prefix = ColorCyan + "● " + ColorReset
	}
	if disabled {
		return prefix + ColorDim + text + ColorReset
	}
	return prefix + text
}

// hintRow renders a dim one-line hint for the shortcuts bar.
func hintRow(text string) string {
	return ColorDim + text + ColorReset
}

// fuzzyMatchScore is a tiny fuzzy-score for palette filtering: nonzero when
// every rune of `needle` appears in `haystack` in order. Higher is better.
// Rune-aware so CJK/标签 labels match correctly (not byte-indexed).
func fuzzyMatchScore(needle, haystack string) int {
	needleRunes := []rune(strings.ToLower(needle))
	hayRunes := []rune(strings.ToLower(haystack))
	if len(needleRunes) == 0 {
		return 0
	}
	if strings.Contains(string(hayRunes), string(needleRunes)) {
		// Substring match scores highest; a prefix match outranks a mid-string
		// substring (so "plan" beats "plugin" for query "pl").
		score := 1000
		if strings.HasPrefix(string(hayRunes), string(needleRunes)) {
			score += 500
		}
		return score + (len(hayRunes) - len(needleRunes))
	}
	score := 0
	hi := 0
	for _, r := range needleRunes {
		matched := -1
		for hi < len(hayRunes) && hayRunes[hi] != r {
			hi++
		}
		if hi >= len(hayRunes) {
			return -1
		}
		matched = hi
		hi++
		score += 10 - minInt(9, hi) // proximity bonus
		if matched == 0 {
			score += 20 // prefix bonus
		}
	}
	return score
}

// paletteItem is one command-palette entry (a runnable action).
type paletteItem struct {
	Label   string // display label
	Detail  string // secondary text (e.g. key binding / slash path)
	Run     func() // executes when chosen
	Visible bool   // false to hide (e.g. future)
}

// filterPalette fuzzy-filters and ranks items by query.
func filterPalette(items []paletteItem, q string) []paletteItem {
	if strings.TrimSpace(q) == "" {
		return items
	}
	type scored struct {
		item  paletteItem
		score int
	}
	var out []scored
	for _, it := range items {
		s := fuzzyMatchScore(q, it.Label)
		if s < 0 {
			continue
		}
		out = append(out, scored{item: it, score: s})
	}
	// stable sort by score desc, then label asc
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && (out[j-1].score < out[j].score ||
			(out[j-1].score == out[j].score && out[j-1].item.Label > out[j].item.Label)); j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	res := make([]paletteItem, len(out))
	for i, s := range out {
		res[i] = s.item
	}
	return res
}

// renderPalette renders the command palette popup (FuzzyPicker style).
func renderPalette(items []paletteItem, q string, selected, width int) []string {
	matches := filterPalette(items, q)
	// Query row at the top so the user always sees what they're filtering by.
	var rows []string
	rows = append(rows, divider(width, "Command Palette", ""))
	rows = append(rows, ColorBold+"? "+ColorReset+colorIfEmpty(q, ColorDim+"type to filter"+ColorReset)+
		ColorGray+"  ("+strconvItoa(len(matches))+" actions)"+ColorReset)
	if len(matches) == 0 {
		rows = append(rows, ColorDim+"(no matching actions)"+ColorReset)
		return rows
	}
	maxRows := 10
	start := 0
	end := len(matches)
	if end-start > maxRows {
		start = selected - maxRows/2
		if start < 0 {
			start = 0
		}
		end = start + maxRows
		if end > len(matches) {
			end = len(matches)
			start = end - maxRows
		}
	}
	for i := start; i < end; i++ {
		it := matches[i]
		label := it.Label
		detail := it.Detail
		if detail != "" {
			detail = "  " + ColorGray + truncateWidthPlain(detail, maxInt(8, width-len(label)-6)) + ColorReset
		}
		text := label + detail
		if i == selected {
			text = ColorReverse + "▸ " + trunreuse(text, width-2) + ColorReset
		} else {
			text = "  " + trunreuse(text, width-2)
		}
		rows = append(rows, text)
	}
	rows = append(rows, ColorGray+truncateWidthPlain(
		"↑↓ 选择 · Enter 执行 · Esc 关闭", width-4)+ColorReset)
	return rows
}

func colorIfEmpty(s string, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// trunreuse truncates s to width columns (plain, no ANSI parsing beyond SGR).
func trunreuse(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if stringWidth(s) <= width {
		return s
	}
	limit := width - 1
	w := 0
	var b strings.Builder
	for _, r := range s {
		rw := runeWidth(r)
		if w+rw > limit {
			break
		}
		w += rw
		b.WriteRune(r)
	}
	b.WriteRune('…')
	return b.String()
}

// ensure package-level rune-width helpers referenced are compiled.

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
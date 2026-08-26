package tui

import (
	"sort"
	"strings"
)

// commandInfo is one completable slash command: the shared registry entries
// plus TUI-local ones merged into the same shape.
type commandInfo struct {
	Name        string
	Description string
}

// maxCompletionRows caps the popup height; longer lists window around the
// selection.
const maxCompletionRows = 8

// filterCommands returns commands whose name starts with the given prefix
// (case-insensitive), sorted by name. Exact matches sort first so a single
// precise candidate is always selected[0].
func filterCommands(prefix string, defs []commandInfo) []commandInfo {
	prefix = strings.ToLower(prefix)
	out := make([]commandInfo, 0, len(defs))
	for _, d := range defs {
		if strings.HasPrefix(strings.ToLower(d.Name), prefix) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// completionToken decides whether the buffer text (up to the cursor) opens a
// completion popup and what token is being completed. A popup only appears
// for a first-word "/prefix" — after a space or inside multi-line input the
// user is writing arguments or prose, not invoking.
func completionToken(text string) (token string, ok bool) {
	if !strings.HasPrefix(text, "/") || strings.ContainsAny(text, " \t\n") {
		return "", false
	}
	return text[1:], true
}

// renderCompletionPopup draws the candidate list as one string of rows joined
// with newlines. The selected row is inverse-video; within every row the
// matched prefix of the name is bolded. A trailing dim hint row documents the
// keys (自解释). Rows wider than the terminal are truncated with ellipsis.
func renderCompletionPopup(matches []commandInfo, selected, width int) string {
	if len(matches) == 0 {
		return ""
	}
	start := 0
	end := len(matches)
	if end-start > maxCompletionRows {
		// Window around the selection, clamped to list bounds.
		start = selected - maxCompletionRows/2
		if start < 0 {
			start = 0
		}
		end = start + maxCompletionRows
		if end > len(matches) {
			end = len(matches)
			start = end - maxCompletionRows
		}
	}
	nameCol := 2 // width of the "/name" column, grows with longest visible name
	for _, m := range matches[start:end] {
		if l := len(m.Name) + 1; l > nameCol {
			nameCol = l
		}
	}

	var rows []string
	for i := start; i < end; i++ {
		m := matches[i]
		selMark := "  "
		if i == selected {
			selMark = ColorReverse + "▸" + ColorReset + " "
		}
		name := highlightPrefix("/"+m.Name, m.Name)
		pad := strings.Repeat(" ", nameCol-len(m.Name))
		desc := truncateWidthPlain(m.Description, width-nameCol-6)
		rows = append(rows, selMark+name+pad+" "+desc)
	}
	rows = append(rows, ColorGray+truncateWidthPlain(
		"Tab/Enter 完成 · ↑↓ 选择 · Esc 关闭", width-4)+ColorReset)
	return strings.Join(rows, "\n")
}

// highlightPrefix renders s ("/name") with the matched prefix — slash plus
// typed characters — bolded so typed vs suggested parts are distinguishable.
func highlightPrefix(s, name string) string {
	runes := []rune(s)
	n := len([]rune(name)) + 1 // +1 for the leading '/'
	if n > len(runes) {
		n = len(runes)
	}
	return ColorBold + string(runes[:n]) + ColorReset + string(runes[n:])
}

// truncateWidthPlain truncates to display width without ANSI awareness of
// colors (popup descriptions are plain) but reusing rune widths.
func truncateWidthPlain(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if w := stringWidth(s); w <= n {
		return s
	}
	limit := n - 1
	w := 0
	out := strings.Builder{}
	for _, r := range s {
		rw := runeWidth(r)
		if w+rw > limit {
			break
		}
		w += rw
		out.WriteRune(r)
	}
	out.WriteRune('…')
	return out.String()
}

// acceptCompletion replaces the current "/tok" token with "/name " and
// returns the new buffer content. tokenStart is the index of '/' in buf.
func acceptCompletion(buf, name string) string {
	return "/" + name + " "
}

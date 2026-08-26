package tui

import (
	"fmt"
	"strings"

	"dsh-go/pkg/llm"
)

// StatusData is one status-bar snapshot. All fields are plain values so the
// formatter stays a pure function.
type StatusData struct {
	Model       string  // current model id (e.g. "deepseek-chat")
	Effort      string  // "" | "off" | "low" | "high" | "max"
	CacheRate   float64 // percent 0..100; negative = no usage yet
	TotalTokens int     // cumulative conversation tokens (in+out+cache)
	Busy        bool    // a turn is streaming
	// Hint 是瞬态警示文案（如 Ctrl+C 双击退出倒计时）；空串不占位。
	Hint string
}

// FormatStatusBar renders the single-line bar:
//
//	deepseek-chat │ think:high │ cache 87% · 12.4k tok │ ● busy
//
// Narrow-terminal degradation drops segments right-to-left (busy dot is kept
// last because it answers "is it working?" first); the model name truncates
// with ellipsis before anything else gets dropped. The result never exceeds
// width cells.
func FormatStatusBar(d StatusData, width int) string {
	if width < 8 {
		width = 8
	}
	const sep = ColorGray + " │ " + ColorReset

	model := d.Model
	if model == "" {
		model = "unknown"
	}
	modelSeg := ColorBold + truncateWidthPlain(model, maxInt(6, width-2)) + ColorReset
	segs := []string{modelSeg}
	if width >= 24 {
		segs = append(segs, formatEffortSeg(d.Effort))
	}
	if width >= 40 {
		segs = append(segs, formatCacheSeg(d.CacheRate, d.TotalTokens))
	}
	if d.Hint != "" {
		// The hint outranks the ready/busy dot: it answers "what will my next
		// keypress do" while it is showing.
		segs = append(segs, ColorYellow+d.Hint+ColorReset)
	} else if width >= 14 {
		segs = append(segs, formatStateSeg(d.Busy))
	}

	out := strings.Join(segs, sep)
	for len(segs) > 1 && stringWidth(out) > width {
		segs = segs[:len(segs)-1]
		out = strings.Join(segs, sep)
	}
	if stringWidth(out) > width {
		out = truncateANSI(out, width)
	}
	return out
}

func formatEffortSeg(effort string) string {
	e := effort
	color := ColorMagenta
	switch effort {
	case "":
		e = llm.DefaultReasoningEffort
	case "off":
		color = ColorGray
	case "low":
		color = ColorCyan
	case "max":
		color = ColorYellow
	}
	return ColorGray + "think:" + ColorReset + color + e + ColorReset
}

func formatCacheSeg(rate float64, totalTokens int) string {
	rateTxt := "–"
	if rate >= 0 {
		rateTxt = fmt.Sprintf("%.0f%%", rate)
	}
	return ColorGray + "cache " + ColorReset + ColorCyan + rateTxt + ColorReset +
		ColorGray + " · " + ColorReset + ColorDim + formatTokenCount(totalTokens) + " tok" + ColorReset
}

func formatStateSeg(busy bool) string {
	if busy {
		return ColorGreen + "● busy" + ColorReset
	}
	return ColorGray + "○ ready" + ColorReset
}

// formatTokenCount renders counts compactly: 980 → "980", 12400 → "12.4k",
// 3250000 → "3.3M".
func formatTokenCount(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		if n < 10_000 {
			return fmt.Sprintf("%.1fk", float64(n)/1000)
		}
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// truncateANSI cuts an ANSI-decorated string to width cells without breaking
// escape sequences (used as the status bar's last-resort clamp).
func truncateANSI(s string, width int) string {
	runes := []rune(s)
	w := 0
	var b strings.Builder
	for i := 0; i < len(runes); i++ {
		if runes[i] == 0x1B {
			j := skipANSI(runes, i)
			b.WriteString(string(runes[i : j+1]))
			i = j
			continue
		}
		rw := runeWidth(runes[i])
		if w+rw > width {
			break
		}
		w += rw
		b.WriteRune(runes[i])
	}
	return b.String()
}

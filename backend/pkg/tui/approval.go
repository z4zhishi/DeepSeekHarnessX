package tui

import (
	"fmt"
	"strconv"
	"strings"

	"dsh-go/pkg/tools"
)

type approvalRequest struct {
	prompt   string
	options  []string
	decision chan tools.ApprovalDecision
}

func isStandardApproval(options []string) bool {
	if len(options) == 0 {
		return true
	}
	for _, o := range options {
		switch normOption(o) {
		case "allow_once", "allow", "once", "allow_all", "always", "deny", "reject", "cancel", "yes", "no":
		default:
			return false
		}
	}
	return true
}

func formatApproval(req approvalRequest) string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(ColorYellow)
	b.WriteString("[Permission Required]")
	b.WriteString(ColorReset)
	b.WriteString(" ")
	b.WriteString(truncateRunes(req.prompt, 600))
	b.WriteString("\n")
	if isStandardApproval(req.options) {
		b.WriteString("  ")
		b.WriteString(ColorGreen)
		b.WriteString("y")
		b.WriteString(ColorReset)
		b.WriteString(" = allow once   ")
		b.WriteString(ColorRed)
		b.WriteString("n")
		b.WriteString(ColorReset)
		b.WriteString(" = deny   ")
		b.WriteString(ColorMagenta)
		b.WriteString("a")
		b.WriteString(ColorReset)
		b.WriteString(" = always   ")
		b.WriteString(ColorCyan)
		b.WriteString("c")
		b.WriteString(ColorReset)
		b.WriteString(" = cancel  (keys apply immediately, no Enter)\n")
		for _, opt := range req.options {
			b.WriteString("  optionId: ")
			b.WriteString(opt)
			b.WriteString("\n")
		}
	} else {
		for i, opt := range req.options {
			fmt.Fprintf(&b, "  %s%d)%s %s\n", ColorCyan, i+1, ColorReset, opt)
		}
		b.WriteString("  enter 1..n, the option id, or y/n/c\n")
	}
	// No trailing "? " prompt: the raw-mode editor renders its own live
	// prompt, and the legacy path appends one explicitly.
	return b.String()
}

func parseApproval(line string, options []string) (tools.ApprovalDecision, bool) {
	s := strings.TrimSpace(line)
	if s == "" {
		return "", false
	}
	lower := strings.ToLower(s)
	switch lower {
	case "y", "yes", "allow", "once", "allow_once", "allow-once":
		return tools.ApprovalAllowOnce, true
	case "a", "always", "allow_all", "allow-all":
		return tools.ApprovalAllowAll, true
	case "n", "no", "deny", "reject", "reject-once":
		return tools.ApprovalDeny, true
	case "c", "cancel":
		return tools.ApprovalCancel, true
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(options) {
		return decisionFromOption(options[n-1], n-1), true
	}
	for i, opt := range options {
		if strings.EqualFold(opt, s) {
			return decisionFromOption(opt, i), true
		}
	}
	return "", false
}

func decisionFromOption(opt string, index int) tools.ApprovalDecision {
	switch normOption(opt) {
	case "allow_once", "allow", "once", "yes":
		return tools.ApprovalAllowOnce
	case "allow_all", "always":
		return tools.ApprovalAllowAll
	case "deny", "reject", "no":
		return tools.ApprovalDeny
	case "cancel":
		return tools.ApprovalCancel
	}
	if index == 0 {
		return tools.ApprovalAllowOnce
	}
	if index == 1 {
		return tools.ApprovalDeny
	}
	return tools.ApprovalCancel
}

func normOption(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", "_"))
}

// truncateRunes keeps a head+tail of s so a huge tool-args prompt cannot
// flood the TUI. n is the max rune count of the result (ellipsis included).
func truncateRunes(s string, n int) string {
	if n <= 8 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	head := (n - 1) / 2
	tail := n - 1 - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

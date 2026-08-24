package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"dsh-go/pkg/llm"
	"dsh-go/pkg/session"
)

const (
	ColorReset   = "\033[0m"
	ColorBold    = "\033[1m"
	ColorDim     = "\033[2m"
	ColorRed     = "\033[31m"
	ColorGreen   = "\033[32m"
	ColorYellow  = "\033[33m"
	ColorBlue    = "\033[34m"
	ColorMagenta = "\033[35m"
	ColorCyan    = "\033[36m"
	ColorGray    = "\033[90m"
)

const (
	maxToolResultChars = 4000
	maxToolArgsChars   = 240
	promptStr          = ColorBold + "> " + ColorReset
)

type frameKind int

const (
	frameWrite frameKind = iota
	framePrompt
	frameConsumed
	frameClear
)

type frame struct {
	kind frameKind
	text string
}

// UI serializes all stdout writes onto one goroutine so stream text never
// interleaves with the prompt.
type UI struct {
	ch        chan frame
	done      chan struct{}
	out       io.Writer
	wg        sync.WaitGroup
	closeOnce sync.Once
}

func newUI(out io.Writer) *UI {
	u := &UI{ch: make(chan frame, 256), done: make(chan struct{}), out: out}
	u.wg.Add(1)
	go u.loop()
	return u
}

func (u *UI) loop() {
	defer u.wg.Done()
	promptShown := false
	for {
		select {
		case <-u.done:
			return
		case f := <-u.ch:
			switch f.kind {
			case frameWrite:
				if promptShown {
					fmt.Fprint(u.out, "\r\033[2K")
					promptShown = false
				}
				fmt.Fprint(u.out, f.text)
			case frameConsumed:
				promptShown = false
			case framePrompt:
				if !promptShown {
					fmt.Fprint(u.out, promptStr)
					promptShown = true
				}
			case frameClear:
				fmt.Fprint(u.out, "\033[2J\033[H")
				promptShown = false
			}
		}
	}
}

func (u *UI) send(f frame) {
	select {
	case u.ch <- f:
	case <-u.done:
	}
}

func (u *UI) write(s string) {
	if s == "" {
		return
	}
	u.send(frame{kind: frameWrite, text: s})
}

func (u *UI) prompt() {
	u.send(frame{kind: framePrompt})
}

func (u *UI) consumed() {
	u.send(frame{kind: frameConsumed})
}

func (u *UI) clear() {
	u.send(frame{kind: frameClear})
}

func (u *UI) close() {
	u.closeOnce.Do(func() { close(u.done) })
	u.wg.Wait()
}

func banner() string {
	var b strings.Builder
	b.WriteString(ColorBold)
	b.WriteString(ColorCyan)
	b.WriteString("DSHX")
	b.WriteString(ColorReset)
	b.WriteString("\n")
	b.WriteString(ColorDim)
	b.WriteString("DeepSeekHarnessX terminal")
	b.WriteString(ColorReset)
	b.WriteString("\n")
	b.WriteString("Type instructions, ")
	b.WriteString(ColorYellow)
	b.WriteString("/help")
	b.WriteString(ColorReset)
	b.WriteString(", or ")
	b.WriteString(ColorRed)
	b.WriteString("/exit")
	b.WriteString(ColorReset)
	b.WriteString(".\n\n")
	return b.String()
}

func formatEnvelope(env *session.SessionEnvelope) (text string, promptAfter bool) {
	if env == nil {
		return "", false
	}
	switch env.Type {
	case session.EventTurnStart:
		return fmt.Sprintf("\n%s[Turn Start]%s\n", ColorCyan, ColorReset), false
	case session.EventUserMessage:
		var msg session.UserMessagePayload
		_ = json.Unmarshal(env.Data, &msg)
		var text string
		for _, b := range msg.Content {
			if b.Type == "text" {
				text += b.Text
			}
		}
		if text == "" {
			return "", false
		}
		return fmt.Sprintf("%sYou:%s %s\n", ColorBold, ColorReset, text), false
	case session.EventAssistantChunk:
		var chunkPayload struct {
			Chunk llm.StreamChunk `json:"chunk"`
		}
		_ = json.Unmarshal(env.Data, &chunkPayload)
		switch chunkPayload.Chunk.Type {
		case llm.ChunkTextDelta:
			return ColorGreen + chunkPayload.Chunk.Text + ColorReset, false
		case llm.ChunkReasoningDelta:
			return ColorGray + chunkPayload.Chunk.Text + ColorReset, false
		}
		return "", false
	case session.EventToolCall:
		var tc session.ToolCallPayload
		_ = json.Unmarshal(env.Data, &tc)
		args := truncRunes(tc.Arguments, maxToolArgsChars)
		return fmt.Sprintf("\n%s[Tool Call] %s %s%s\n", ColorYellow, tc.Name, args, ColorReset), false
	case session.EventToolResult:
		return formatToolResult(env), false
	case session.EventTurnEnd:
		return fmt.Sprintf("%s\n[Turn Completed]%s\n\n", ColorDim, ColorReset), true
	default:
		return "", false
	}
}

func formatToolResult(env *session.SessionEnvelope) string {
	var tr session.ToolResultPayload
	_ = json.Unmarshal(env.Data, &tr)
	body := toolResultText(tr)
	if body == "" {
		body = "(no text output)"
	}
	body = truncRunes(body, maxToolResultChars)
	if (tr.View != nil && tr.View.Kind == "diff") || looksLikeUnifiedDiff(body) {
		body = colorDiff(body)
	}
	status := "OK"
	color := ColorBlue
	if tr.Error != nil {
		status = "ERROR"
		color = ColorRed
	}
	return fmt.Sprintf("%s[Tool %s]%s\n%s\n", color, status, ColorReset, body)
}

func toolResultText(tr session.ToolResultPayload) string {
	var b strings.Builder
	var walk func(blocks []session.ContentBlock)
	walk = func(blocks []session.ContentBlock) {
		for _, c := range blocks {
			if c.Type == "text" || c.Type == "reasoning" {
				b.WriteString(c.Text)
			}
			if len(c.Content) > 0 {
				walk(c.Content)
			}
		}
	}
	walk(tr.Message.Content)
	if b.Len() == 0 && tr.View != nil && tr.View.Text != "" {
		return tr.View.Text
	}
	return b.String()
}

func looksLikeUnifiedDiff(s string) bool {
	if strings.HasPrefix(s, "diff --git ") || strings.Contains(s, "\ndiff --git ") {
		return true
	}
	hasAt, hasPlus, hasMinus := false, false, false
	for _, line := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"):
			hasAt = true
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			hasPlus = true
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			hasMinus = true
		}
	}
	return hasAt && (hasPlus || hasMinus)
}

func colorDiff(s string) string {
	var b strings.Builder
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			b.WriteString(ColorBold)
			b.WriteString(line)
			b.WriteString(ColorReset)
		case strings.HasPrefix(line, "+"):
			b.WriteString(ColorGreen)
			b.WriteString(line)
			b.WriteString(ColorReset)
		case strings.HasPrefix(line, "-"):
			b.WriteString(ColorRed)
			b.WriteString(line)
			b.WriteString(ColorReset)
		case strings.HasPrefix(line, "@@"):
			b.WriteString(ColorCyan)
			b.WriteString(line)
			b.WriteString(ColorReset)
		default:
			b.WriteString(line)
		}
	}
	return b.String()
}

func truncRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

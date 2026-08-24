package tui

import (
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"dsh-go/pkg/agent"
	"dsh-go/pkg/gateway"
	"dsh-go/pkg/llm"
	"dsh-go/pkg/session"
	"dsh-go/pkg/storage"
	"dsh-go/pkg/tools"
)

func enterAltScreen() {
	enableVT()
	fmt.Fprint(os.Stdout, "\033[?1049h\033[2J\033[H")
}

func leaveAltScreen() {
	fmt.Fprint(os.Stdout, "\033[?1049l")
}

// RunTUI launches the native terminal interactive mode.
func RunTUI(store gateway.SessionStore, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter, modelName string) {
	enterAltScreen()
	defer leaveAltScreen()

	ui := newUI(os.Stdout)
	defer ui.close()

	sessionID := fmt.Sprintf("tui-%d", time.Now().UnixNano())
	header := session.SessionHeader{
		ID:        sessionID,
		CreatedAt: time.Now().UnixMilli(),
		Cwd:       ".",
	}

	ringBuf := storage.NewRingBuffer(512)
	ag := agent.NewAgent(header, ringBuf, nil, store, toolReg, adapter, "You are DSHX Assistant.", modelName)
	approvalCh := make(chan approvalRequest, 4)
	ag.RequestUser = func(prompt string, options []string) (tools.ApprovalDecision, error) {
		req := approvalRequest{
			prompt:   prompt,
			options:  options,
			decision: make(chan tools.ApprovalDecision, 1),
		}
		approvalCh <- req
		return <-req.decision, nil
	}
	eventsChan := ag.Subscribe()
	ag.Start()
	defer ag.Stop()

	go func() {
		for env := range eventsChan {
			text, promptAfter := formatEnvelope(env)
			if text != "" {
				ui.write(text)
			}
			if promptAfter {
				ui.prompt()
			}
		}
	}()

	inputCh := startStdinPump()
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)

	ui.write(banner())
	ui.prompt()

	for {
		select {
		case <-interrupt:
			ui.write("\nExiting DSHX TUI.\n")
			return
		case req := <-approvalCh:
			ui.write(formatApproval(req))
			if !readApproval(inputCh, ui, req) {
				return
			}
		case line, ok := <-inputCh:
			if !ok {
				return
			}
			ui.consumed()
			line = strings.TrimSpace(line)
			if line == "" {
				ui.prompt()
				continue
			}
			if line == "/exit" || line == "/quit" {
				ui.write("Exiting DSHX TUI.\n")
				return
			}
			if line == "/help" {
				ui.write(helpText(toolReg))
				ui.prompt()
				continue
			}
			if line == "/clear" {
				ui.clear()
				ui.write(banner())
				ui.prompt()
				continue
			}
			if strings.HasPrefix(line, "/") && toolReg != nil && toolReg.Commands != nil {
				if res := toolReg.Commands.Execute(tools.CommandInvocation{
					SessionID: sessionID,
					Cwd:       header.Cwd,
					Emit: func(eventType string, payload any) {
						_, _ = ag.EmitEvent(eventType, payload)
					},
					EmitSeq: func(eventType string, payload any) (int, error) {
						env, err := ag.EmitEvent(eventType, payload)
						if err != nil {
							return 0, err
						}
						return env.Seq, nil
					},
					Policy: toolReg.Policy,
				}, line); res != nil {
					if res.Text == "exit" {
						ui.write("Exiting DSHX TUI.\n")
						return
					}
					if res.Text != "" {
						ui.write(ColorCyan + res.Text + ColorReset)
						if !strings.HasSuffix(res.Text, "\n") {
							ui.write("\n")
						}
					}
					ui.prompt()
					continue
				}
			}
			ag.PostUserMessage(session.UserMessagePayload{
				ID:   fmt.Sprintf("tui-msg-%d", time.Now().UnixNano()),
				Role: "user",
				Content: []session.ContentBlock{
					{Type: "text", Text: line},
				},
				Source: session.MessageSource{Kind: "user"},
			})
		}
	}
}

func readApproval(inputCh <-chan string, ui *UI, req approvalRequest) bool {
	for {
		line, ok := <-inputCh
		if !ok {
			req.decision <- tools.ApprovalCancel
			return false
		}
		ui.consumed()
		if d, ok := parseApproval(line, req.options); ok {
			req.decision <- d
			return true
		}
		ui.write("  无效选择，请输入 y / n / c 或选项编号/id\n" + ColorBold + "? " + ColorReset)
	}
}

func helpText(toolReg *tools.ToolRegistry) string {
	var b strings.Builder
	b.WriteString("\nCommands:\n")
	b.WriteString("  /help     Show this help\n")
	b.WriteString("  /clear    Clear the screen\n")
	b.WriteString("  /exit     Leave TUI (/quit)\n")
	if toolReg != nil && toolReg.Commands != nil {
		defs := toolReg.Commands.List()
		sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
		for _, d := range defs {
			if d.Name == "help" || d.Name == "exit" {
				continue
			}
			fmt.Fprintf(&b, "  /%-8s %s\n", d.Name, d.Description)
		}
	}
	b.WriteString("\nApprovals: y = allow once, n = deny, c = cancel; or optionId / 1..n\n")
	return b.String()
}

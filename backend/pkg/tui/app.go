package tui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"dsh-go/pkg/agent"
	"dsh-go/pkg/gateway"
	"dsh-go/pkg/llm"
	"dsh-go/pkg/session"
	"dsh-go/pkg/storage"
	"dsh-go/pkg/tools"
)

// Terminal colors using standard ANSI escape codes for zero-dependency instant startup
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

// approvalCh carries permission requests from agent goroutines to the TUI main
// loop (single-reader stdin design). inputCh is the single stdin pump consumed
// by both message entry and approval prompts.
var approvalCh = make(chan approvalRequest, 4)
var inputCh = make(chan string, 64)

// approvalRequest is one in-flight permission request handed from the agent
// goroutine to the interactive TUI main loop. The main loop reads the user's
// decision from stdin and resolves the channel (y=allow once / n=deny /
// c=cancel).
type approvalRequest struct {
	prompt   string
	decision chan tools.ApprovalDecision
}

// RunTUI launches the ultra-fast native terminal interactive mode.
func RunTUI(store gateway.SessionStore, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter, modelName string) {
	printBanner()

	sessionID := fmt.Sprintf("tui-%d", time.Now().UnixNano())
	header := session.SessionHeader{
		ID:        sessionID,
		CreatedAt: time.Now().UnixMilli(),
		Cwd:       ".",
	}

	ringBuf := storage.NewRingBuffer(512)
	ag := agent.NewAgent(header, ringBuf, nil, store, toolReg, adapter, "You are DSH Terminal Assistant.", modelName)
	ag.RequestUser = func(prompt string, options []string) (tools.ApprovalDecision, error) {
		// Run the approval prompt through the main loop so the single stdin
		// reader owns all input; the loop replies on req.decision.
		req := approvalRequest{prompt: prompt, decision: make(chan tools.ApprovalDecision, 1)}
		approvalCh <- req
		return <-req.decision, nil
	}
	eventsChan := ag.Subscribe()
	ag.Start()
	defer ag.Stop()

	// Goroutine to render streaming event frames in real time
	go func() {
		for env := range eventsChan {
			switch env.Type {
			case session.EventTurnStart:
				fmt.Printf("\n%s[Turn Start]%s\n", ColorCyan, ColorReset)

			case session.EventAssistantChunk:
				// 流式文本增量直刷（assistant/chunk → llm.StreamChunk）
				var chunkPayload struct {
					Chunk llm.StreamChunk `json:"chunk"`
				}
				_ = json.Unmarshal(env.Data, &chunkPayload)
				switch chunkPayload.Chunk.Type {
				case llm.ChunkTextDelta:
					fmt.Print(ColorGreen + chunkPayload.Chunk.Text + ColorReset)
				case llm.ChunkReasoningDelta:
					fmt.Print(ColorGray + chunkPayload.Chunk.Text + ColorReset)
				}

			case session.EventToolCall:
				var tc session.ToolCallPayload
				_ = json.Unmarshal(env.Data, &tc)
				fmt.Printf("\n%s[Tool Call] %s %s%s\n", ColorYellow, tc.Name, tc.Arguments, ColorReset)

			case session.EventToolResult:
				var tr session.ToolResultPayload
				_ = json.Unmarshal(env.Data, &tr)
				var text string
				for _, b := range tr.Message.Content {
					if b.Type == "text" {
						text += b.Text
					}
				}
				if text == "" {
					text = "(no text output)"
				}
				status := "OK"
				if tr.Error != nil {
					status = "ERROR"
				}
				fmt.Printf("%s[Tool %s] %s%s\n", ColorBlue, status, text, ColorReset)

			case session.EventTurnEnd:
				fmt.Printf("%s\n[Turn Completed]%s\n\n%s> %s", ColorDim, ColorReset, ColorBold, ColorReset)
			}
		}
	}()

	// Input pump: one reader goroutine owns stdin; the main loop consumes
	// both user lines and approval requests from the single channel.
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			inputCh <- scanner.Text()
		}
	}()

	fmt.Printf("%s> %s", ColorBold, ColorReset)
	for {
		select {
		case line := <-inputCh:
			line = strings.TrimSpace(line)
			if line == "" {
				fmt.Printf("%s> %s", ColorBold, ColorReset)
				continue
			}
			if line == "/exit" || line == "/quit" {
				fmt.Println("Exiting DSH TUI.")
				return
			}
			if line == "/help" {
				printHelp()
				fmt.Printf("%s> %s", ColorBold, ColorReset)
				continue
			}
			if line == "/clear" {
				fmt.Print("\033[2J\033[H")
				printBanner()
				fmt.Printf("%s> %s", ColorBold, ColorReset)
				continue
			}
			// Resolve known slash commands through the shared registry: the
			// command/run -> command/done lifecycle lands in the session log
			// (upstream dsh-commands), so the TUI, gateway RPC and Godot share
			// one command table. Unknown /lines fall through to the model.
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
					if res.Text != "" && res.Text != "exit" {
						fmt.Printf("%s%s%s\n", ColorCyan, res.Text, ColorReset)
					}
					if res.Text == "exit" {
						fmt.Println("Exiting DSH TUI.")
						return
					}
					fmt.Printf("%s> %s", ColorBold, ColorReset)
					continue
				}
			}
			// Post prompt to agent turn loop
			ag.PostUserMessage(session.UserMessagePayload{
				ID:   fmt.Sprintf("tui-msg-%d", time.Now().UnixNano()),
				Role: "user",
				Content: []session.ContentBlock{
					{Type: "text", Text: line},
				},
				Source: session.MessageSource{Kind: "user"},
			})
		case req := <-approvalCh:
			interactiveApproval(req)
		}
	}
}

// interactiveApproval prints one permission prompt and reads y/n/c from the
// TUI input channel, blocking until a valid decision is entered.
func interactiveApproval(req approvalRequest) {
	fmt.Printf("\n%s[Permission Required]%s %s\n", ColorYellow, ColorReset, req.prompt)
	fmt.Printf("  %sy%s = allow once   %sn%s = deny   %sc%s = cancel%s\n> ",
		ColorGreen, ColorReset, ColorRed, ColorReset, ColorMagenta, ColorReset, ColorReset)
	for {
		line := <-inputCh
		line = strings.TrimSpace(strings.ToLower(line))
		switch line {
		case "y", "yes", "allow", "once":
			req.decision <- tools.ApprovalAllowOnce
			fmt.Printf("%s> %s", ColorBold, ColorReset)
			return
		case "n", "no", "deny", "reject":
			req.decision <- tools.ApprovalDeny
			fmt.Printf("%s> %s", ColorBold, ColorReset)
			return
		case "c", "cancel":
			req.decision <- tools.ApprovalCancel
			fmt.Printf("%s> %s", ColorBold, ColorReset)
			return
		default:
			fmt.Print("  无效选择，请输入 y / n / c: ")
		}
	}
}

func printBanner() {
	fmt.Printf("%s%s=== DeepSeek-Harness (DSH) Native Terminal TUI ===%s\n", ColorBold, ColorCyan, ColorReset)
	fmt.Printf("%sPowered by Go 1.25 + Event Sourcing + Actor Engine%s\n", ColorDim, ColorReset)
	fmt.Printf("Type your instructions or %s/help%s for commands, %s to quit.\n\n", ColorYellow, ColorReset, ColorRed+"/exit"+ColorReset)
}

func printHelp() {
	fmt.Println("\nAvailable Commands:")
	fmt.Println("  /help     - Show this help message")
	fmt.Println("  /clear    - Clear screen")
	fmt.Println("  /exit     - Exit TUI mode")
	fmt.Println("\nTool Permission Prompts:")
	fmt.Println("  y - Allow once")
	fmt.Println("  n - Deny (reject once)")
	fmt.Println("  c - Cancel")
}

package tui

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
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

// RunTUI launches the native terminal interactive mode. Two input stacks:
// a raw-mode editor (real-time completion, history, status bar) whenever the
// terminal supports it, and the legacy cooked line scanner as fallback for
// piped/non-TTY stdin — byte-for-byte the pre-overhaul behavior. instructions
// is the resolved workspace instruction text ("" = none) appended to every
// TUI session's system prompt.
func RunTUI(store gateway.SessionStore, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter, modelName string, runtime agent.PluginRuntime, instructions string) {
	enterAltScreen()
	exit := false
	if restore, ok := enableRawInput(); ok {
		exit = runInteractiveTUI(store, toolReg, adapter, modelName, restore, runtime, instructions)
	} else {
		exit = runCookedTUI(store, toolReg, adapter, modelName, runtime, instructions)
	}
	leaveAltScreen()
	if exit {
		fmt.Println("Exiting DSHX TUI.")
	}
}

const tuiHistoryEntries = 500

// runInteractiveTUI is the raw-mode session. Returns true when the user asked
// to exit (vs stdin EOF without explicit request — both print the goodbye).
func runInteractiveTUI(store gateway.SessionStore, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter, modelName string, restoreRaw func(), runtime agent.PluginRuntime, instructions string) bool {
	defer restoreRaw()
	fmt.Fprint(os.Stdout, "\033[?2004h") // bracketed paste on
	defer fmt.Fprint(os.Stdout, "\033[?2004l")
	// Mouse reporting (SGR form) so wheel scrolls the scrollback and clicks
	// select entries / focus the prompt (GrokBuild parity). Disabled on exit.
	fmt.Fprint(os.Stdout, "\033[?1000h\033[?1006h")
	defer fmt.Fprint(os.Stdout, "\033[?1000l\033[?1006l")

	sessionID := fmt.Sprintf("tui-%d", time.Now().UnixNano())
	header := session.SessionHeader{
		ID:        sessionID,
		CreatedAt: time.Now().UnixMilli(),
		Cwd:       ".",
	}

	tune := newTunedAdapter(adapter, modelName)

	ringBuf := storage.NewRingBuffer(512)
	ag := agent.NewAgent(header, ringBuf, nil, store, toolReg, tune, "You are DSHX Assistant.", modelName)
	ag.Instructions = instructions
	ag.AttachPluginRuntime(runtime)
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

	defs := mergedCommandDefs(toolReg)
	hist := NewHistory(tuiHistoryEntries)
	histPath, histOK := DefaultHistoryPath()
	if histOK {
		if err := hist.LoadFile(histPath); err != nil && os.Getenv("DSHX_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "[dshx] history load: %v\n", err)
		}
		defer func() { _ = hist.SaveFile(histPath) }()
	}
	ed := newEditor(defs, hist)

	var turnActive atomic.Bool
	var hints hintBar // transient status-bar warnings (Ctrl+C exit window)
	statusFn := func() StatusData {
		u := tune.Totals()
		return StatusData{
			Model:       tune.Model(),
			Effort:      tune.Effort(),
			CacheRate:   u.CacheRate(),
			TotalTokens: u.Total(),
			Busy:        turnActive.Load(),
			Hint:        hints.get(),
		}
	}
	pres := newPresenter(os.Stdout, ed, statusFn)
	defer pres.Close()
	tune.onUsage = pres.scr.Refresh // live cache-rate updates mid-stream

	eventsChan := ag.Subscribe()
	ag.Start()
	defer ag.Stop()

	go func() {
		for env := range eventsChan {
			text, promptAfter := formatEnvelope(env)
			if text != "" {
				pres.WriteKind(text, kindOfEvent(env.Type))
			}
			switch env.Type {
			case session.EventTurnStart:
				turnActive.Store(true)
				pres.RefreshInput()
			case session.EventTurnEnd:
				if turnActive.Swap(false) {
					// A dangling partial line (stream cut mid-delta) would
					// glue the next prompt row onto it visually; terminate it.
					pres.FlushTail()
				}
				pres.RefreshInput()
			}
			if promptAfter {
				pres.RefreshInput()
			}
		}
	}()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)

	cwd, _ := os.Getwd()
	pres.resize()
	pres.Write(banner(tune.Model(), cwd))
	pres.RefreshInput()

	keyCh := startKeyPump()
	resizeTicker := time.NewTicker(200 * time.Millisecond)
	defer resizeTicker.Stop()
	state := statePrompt
	var approval approvalRequest
	approvalDraft := ""     // message being typed when an approval interrupts
	var lastCtrlC time.Time // zero = exit window disarmed
	exitReq := false
	var lastEsc time.Time   // last Esc (for the 2xEsc 800ms clear/rewind window)
	var stash string        // draft stash/pop (Ctrl+S / Alt+S), git-stash style
	var approvalSel int     // Tab/Shift+Tab walk the approval card's options
	var approvalParked bool // Esc parks the keyboard in the scrollback (card stays)
	vimMode := false        // /vim-mode toggles Vim scrollback navigation
	// submitLine runs one palette-chosen command through the same path as a
	// typed line (so /command lifecycle lands in the session log once); a
	// line that asks to exit (e.g. /exit) sets exitReq so the loop returns.
	submitLine := func(line string) {
		if handleSubmit(ag, toolReg, pres, tune, defs, sessionID, header.Cwd, line) {
			exitReq = true
		}
	}
	paletteExit := func() { exitReq = true }
	// cleanApprovalState resets approval-related editor/UI state so transitions
	// stay consistent across enter/ctrlc/esc paths.
	cleanApprovalState := func() {
		state = statePrompt
		pres.CloseApprovalCard()
		approvalSel = 0
		approvalParked = false
		ed.SetPrompt(promptStr)
		ed.SetBuffer(approvalDraft)
		approvalDraft = ""
		pres.ParkCard(false)
		pres.SetFocus(focusPrompt)
		pres.RefreshInput()
	}

	// Leaving while an approval is pending would strand the agent actor on
	// its decision channel; settle it as cancelled on every exit path.
	defer func() {
		if state == stateApproval {
			select {
			case approval.decision <- tools.ApprovalCancel:
			default:
			}
		}
	}()

	// handleCtrlC applies the P1-7 semantics for ONE Ctrl+C event, whichever
	// way it arrived (decoded 0x03 key in raw mode, or the OS signal channel):
	//
	//   - during an approval wait: cancel the approval, never quit;
	//   - otherwise: first strike clears the line / closes popups and arms a
	//     2s exit window with a status-bar hint; a second strike inside the
	//     window quits. Returns true when the program must exit.
	handleCtrlC := func(now time.Time) bool {
		if state == stateApproval {
			// Ctrl+C during an approval means cancel-the-approval, never
			// quit-the-program (same as Esc / 'c').
			approval.decision <- tools.ApprovalCancel
			state = statePrompt
			ed.SetPrompt(promptStr)
			ed.SetBuffer(approvalDraft)
			approvalDraft = ""
			lastCtrlC = time.Time{}
			hints.set("")
			pres.RefreshInput()
			return false
		}
		if turnActive.Load() {
			ag.AbortTurn()
			lastCtrlC = time.Time{}
			hints.set("turn aborted")
			pres.RefreshInput()
			return false
		}
		quit, ref := resolveCtrlC(lastCtrlC, now)
		lastCtrlC = ref
		if quit {
			return true
		}
		// First strike: clear the line / close the completion popup, then
		// warn via a transient status-bar hint.
		ed.Clear()
		gen := hints.set(ctrlCPrompt)
		go func(ref time.Time, gen uint64) {
			time.Sleep(ctrlCExitWindow)
			// Expire only if this window is still current: a newer
			// press bumped the generation and re-armed its own timer.
			if !ctrlCArmed(ref, time.Now()) || !hints.clearIfGen(gen) {
				return
			}
			pres.RefreshInput()
		}(ref, gen)
		pres.RefreshInput()
		return false
	}

	for {
		select {
		case <-interrupt:
			// Raw mode normally swallows SIGINT from Ctrl+C (the key arrives
			// as bytes below); treat any surviving signal identically.
			if handleCtrlC(time.Now()) {
				return true
			}
		case req := <-approvalCh:
			approval = req
			approvalDraft = ed.Buffer() // keep the half-typed message alive
			pres.Write(formatApproval(req))
			// Render the blocking card with the current option highlighted so
			// Tab/↑↓ cycling is self-explanatory. The option list mirrors the
			// backend-provided options exactly so the highlighted index always
			// matches the option Enter confirms (no off-by-one).
			var labels []string
			for _, o := range req.options {
				labels = append(labels, o)
			}
			hint := "y/n/a/c · ↑↓/Tab 选择 · Esc 暂离"
			if !isStandardApproval(req.options) {
				hint = "↑↓/Tab 选择 · 输入编号 · Esc 暂离"
			}
			pres.OpenApprovalCard(truncateRunes(req.prompt, 200), labels, hint)
			ed.SetPrompt(approvalPromptStr)
			ed.Clear()
			pres.RefreshInput()
			state = stateApproval
		case <-resizeTicker.C:
			pres.resize()
		case k, ok := <-keyCh:
			if !ok {
				return true
			}
			pres.resize()
			if k.kind == keyEOF {
				return true
			}
			// Mouse reporting (GrokBuild parity): wheel scrolls the
			// transcript, a left-click on the prompt area focuses it, and a
			// left-click on the scrollback selects the entry under the cursor.
			if k.kind == keyMouse {
				switch {
				case k.mouse.button == 4: // wheel up
					pres.ScrollPage(-1)
					continue
				case k.mouse.button == 5: // wheel down
					pres.ScrollPage(1)
					continue
				case k.mouse.button == 0 && k.mouse.pressed:
					// Left click: leave the scrollback browse mode (if we were
					// in it) and focus the prompt — the common "I want to type
					// now" gesture. Precise click-to-select is a scrollback-mode
					// refinement; keep focus handoff simple and predictable.
					pres.SetFocus(focusPrompt)
					pres.RefreshInput()
					continue
				}
			}
			if k.kind == keyCtrl && k.ctrl == 'c' {
				if handleCtrlC(time.Now()) {
					return true
				}
				continue
			}
			// Blocking permission card (GrokBuild contract): ↑↓ move options,
			// Tab/Shift+Tab walk them in a loop, 1-9 pick directly, Enter
			// confirms the focused option, Ctrl+O turns on always-approve
			// (YOLO), Esc parks focus in the scrollback (card stays pending)
			// — a second Esc cancels the request.
			if state == stateApproval {
				// When the keyboard is parked in the scrollback, only Tab
				// (return to the card) and Esc (cancel) act on the card; every
				// other key navigates the scrollback so the user can read the
				// context behind the pending request.
				if approvalParked {
					switch {
					case k.kind == keyTab:
						approvalParked = false
						pres.ParkCard(false)
						pres.SetFocus(focusPrompt)
						pres.RefreshInput()
						continue
					case k.kind == keyEsc:
						approval.decision <- tools.ApprovalCancel
						cleanApprovalState()
						continue
					}
					// fall through to scrollback navigation below
				} else {
					switch {
					case k.kind == keyUp:
						approvalSel = pres.ApprovalMove(-1)
						pres.RefreshInput()
						continue
					case k.kind == keyDown:
						approvalSel = pres.ApprovalMove(1)
						pres.RefreshInput()
						continue
					case k.kind == keyTab:
						approvalSel = pres.ApprovalMove(1)
						pres.RefreshInput()
						continue
					case k.kind == keyShiftTab:
						approvalSel = pres.ApprovalMove(-1)
						pres.RefreshInput()
						continue
					case k.kind == keyEnter:
						// Confirm the option currently highlighted by ↑↓/Tab.
						opt := ""
						if approvalSel >= 0 && approvalSel < len(approval.options) {
							opt = approval.options[approvalSel]
						}
						if d, ok2 := parseApproval(opt, approval.options); ok2 {
							approval.decision <- d
							cleanApprovalState()
							continue
						}
					case k.kind == keyCtrl && k.ctrl == 'o':
						// YOLO: always-allow this session.
						approval.decision <- tools.ApprovalAllowAll
						cleanApprovalState()
						continue
					case k.kind == keyRune:
						if d, ok2 := parseApproval(string(k.r), approval.options); ok2 {
							approval.decision <- d
							cleanApprovalState()
							continue
						}
					}
					// Non-card keys fall through (e.g. a letter that isn't an
					// option) so Esc below can park the card.
				}
			}
			// Command palette (Ctrl+P) owns the keyboard while open: type to
			// fuzzy-filter, ↑↓ to move, Enter to run, Esc to close. This must
			// gate BEFORE the editor/scrollback handlers so palette keystrokes
			// never fall through into the prompt buffer.
			if pres.PaletteIsOpen() {
				switch {
				case k.kind == keyRune && k.r != 'p':
					pres.PaletteType(k.r)
					continue
				case k.kind == keyBackspace:
					pres.PaletteBackspace()
					continue
				case k.kind == keyUp:
					pres.PaletteMove(-1)
					continue
				case k.kind == keyDown:
					pres.PaletteMove(1)
					continue
				case k.kind == keyEnter:
					pres.PaletteRun()
					if exitReq {
						return true
					}
					continue
				case k.kind == keyCtrl && (k.ctrl == 'c' || k.ctrl == 'p'):
					pres.PaletteClose()
					continue
				case k.kind == keyEsc:
					pres.PaletteClose()
					continue
				}
			}
			// Focus + scrollback navigation (GrokBuild parity). These run
			// before the editor so Tab hands focus between the prompt and the
			// scrollback browse pane, and ↑↓/Shift←→ move a selection while
			// focused on the scrollback. They never apply during an approval
			// wait (the card owns the keyboard).
			if state == statePrompt {
				switch {
				case k.kind == keyPageUp:
					pres.ScrollPage(-1)
					continue
				case k.kind == keyPageDown:
					pres.ScrollPage(1)
					continue
				case k.kind == keyCtrl && k.ctrl == 'p':
					pres.OpenPalette(paletteActions(submitLine, paletteExit))
					continue
				case k.kind == keyTab:
					// Tab toggles focus prompt<->scrollback.
					if pres.Focus() == focusScrollback {
						pres.SetFocus(focusPrompt)
					} else {
						pres.SetFocus(focusScrollback)
					}
					continue
				case k.kind == keyCtrl && k.ctrl == 's':
					// Draft stash/pop (git-stash style): stash a non-empty
					// draft, or restore the stashed one when the prompt is
					// empty. Text and images are stashed; one draft at a time.
					if ed.Empty() {
						if stash != "" {
							ed.SetBuffer(stash)
							stash = ""
							hints.set("draft restored")
						}
					} else {
						stash = ed.Buffer()
						ed.Clear()
						hints.set("draft stashed (Ctrl+S to restore)")
					}
					pres.RefreshInput()
					continue
				}
				// When focused on the scrollback, ↑↓ and Shift+←/→ navigate
				// the selection instead of the editor. In Vim mode, j/k,
				// H/L, h/l, e and E map to navigation / fold commands.
				if pres.Focus() == focusScrollback {
					switch k.kind {
					case keyUp:
						pres.SelectMove(-1)
						continue
					case keyDown:
						pres.SelectMove(1)
						continue
					case keyShiftLeft:
						pres.JumpTurn(-1)
						continue
					case keyShiftRight:
						pres.JumpTurn(1)
						continue
					case keyRune:
						if vimMode {
							switch k.r {
							case 'j':
								pres.SelectMove(1)
								continue
							case 'k':
								pres.SelectMove(-1)
								continue
							case 'L':
								pres.JumpTurn(1)
								continue
							case 'H':
								pres.JumpTurn(-1)
								continue
							case 'h', 'l', 'e':
								pres.ToggleFold()
								continue
							case 'E':
								pres.ToggleAllFolds()
								continue
							}
						}
						continue
					case keyLeft, keyHome, keyPageUp, keyPageDown:
						// fall through to scroll paging below
					}
					continue
				}
			}
			act := ed.HandleKey(k)
			switch {
			case act.Submit != "":
				line := strings.TrimSpace(act.Submit)
				ed.Clear()
				ed.SetPrompt(promptStr)
				if state == stateApproval {
					if d, ok2 := parseApproval(line, approval.options); ok2 {
						approval.decision <- d
						state = statePrompt
						pres.CloseApprovalCard()
						ed.SetPrompt(promptStr)
						ed.SetBuffer(approvalDraft)
						approvalDraft = ""
					} else {
						pres.Write("  无效选择，请输入 y / n / c 或选项编号/id\n")
					}
					pres.RefreshInput()
					continue
				}
				if line == "/vim-mode" {
					vimMode = !vimMode
					pres.Write(ColorCyan + "vim mode " + onOff(vimMode) + ColorReset + "\n")
					pres.RefreshInput()
					continue
				}
				if quit := handleSubmit(ag, toolReg, pres, tune, defs, sessionID, header.Cwd, line); quit {
					return true
				}
				pres.RefreshInput()
			case act.Interrupt:
				// Layered Esc state machine (GrokBuild parity): approval card
				// cancels; a running turn aborts (draft preserved); an idle
				// non-empty draft clears on 2xEsc within 800ms (stashed);
				// an idle empty prompt with messages rewinds on 2xEsc.
				switch {
				case state == stateApproval:
					// Esc first parks the keyboard in the scrollback (the card
					// stays pending so the user can read context); the next
					// Esc cancels the request. Tab returns to the card.
					if !approvalParked {
						approvalParked = true
						pres.ParkCard(true)
						pres.SetFocus(focusScrollback)
					} else {
						approval.decision <- tools.ApprovalCancel
						cleanApprovalState()
					}
				case turnActive.Load():
					// Mid-turn Esc cancels, preserving the draft (unlike
					// Ctrl+C's clear-first gesture).
					ag.AbortTurn()
				case !ed.Empty():
					now := time.Now()
					if now.Sub(lastEsc) <= 800*time.Millisecond {
						// Second press within the window: stash + clear.
						stash = ed.Buffer()
						ed.Clear()
						lastEsc = time.Time{}
						hints.set("draft stashed (Ctrl+S to restore)")
					} else {
						lastEsc = now
						hints.set("press Esc again to clear")
						pres.RefreshInput()
						continue
					}
				default:
					now := time.Now()
					if now.Sub(lastEsc) <= 800*time.Millisecond {
						pres.Write(ColorDim + "  (rewind picker pending — see /rewind)\n" + ColorReset)
						lastEsc = time.Time{}
					} else {
						lastEsc = now
					}
				}
				pres.RefreshInput()
			default:
				pres.RefreshInput()
			}
		}
	}
}

type loopState int

const (
	statePrompt loopState = iota
	stateApproval
)

// handleSubmit dispatches one submitted line. Returns true to exit the TUI.
func handleSubmit(
	ag *agent.Agent,
	toolReg *tools.ToolRegistry,
	pres *presenter,
	tune *tunedAdapter,
	defs []commandInfo,
	sessionID, cwd, line string,
) bool {
	if line == "" {
		return false
	}
	if line == "/exit" || line == "/quit" {
		return true
	}
	if line == "/help" {
		pres.Write(buildHelpText(defs))
		return false
	}
	if line == "/clear" {
		pres.Clear()
		pres.Write(banner(tune.Model(), tuiCwd()))
		return false
	}
	if strings.HasPrefix(line, "/") {
		if handled, out := handleLocalCommand(tune, line); handled {
			if out != "" {
				pres.Write(ColorCyan + out + ColorReset + "\n")
			}
			return false
		}
		if toolReg != nil && toolReg.Commands != nil {
			if res := toolReg.Commands.Execute(tools.CommandInvocation{
				SessionID: sessionID,
				Cwd:       cwd,
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
					return true
				}
				if res.Text != "" {
					pres.Write(ColorCyan + res.Text + ColorReset)
					if !strings.HasSuffix(res.Text, "\n") {
						pres.Write("\n")
					}
				}
				return false
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
		return false
	}

	ag.PostUserMessage(session.UserMessagePayload{
		ID:   fmt.Sprintf("tui-msg-%d", time.Now().UnixNano()),
		Role: "user",
		Content: []session.ContentBlock{
			{Type: "text", Text: line},
		},
		Source: session.MessageSource{Kind: "user"},
	})
	return false
}

// handleLocalCommand implements the TUI-only commands that need process-local
// state (the tuned adapter). They deliberately bypass the shared registry so
// the GUI/gateway surface never sees TUI-internal knobs.
func handleLocalCommand(tune *tunedAdapter, line string) (handled bool, out string) {
	name, raw, ok := ParseCommandLocal(line)
	if !ok {
		return false, ""
	}
	switch name {
	case "thinking":
		arg := strings.TrimSpace(raw)
		switch {
		case arg == "":
			e := tune.CycleEffort()
			return true, fmt.Sprintf("Reasoning effort: %s", e)
		case ValidEffort(arg):
			tune.SetEffort(arg)
			return true, fmt.Sprintf("Reasoning effort: %s", arg)
		default:
			return true, "usage: /thinking [off|low|high|max] (无参数循环切换)"
		}
	case "model":
		arg := strings.TrimSpace(raw)
		if arg == "" {
			return true, fmt.Sprintf("Model: %s", tune.Model())
		}
		tune.SetModel(arg)
		return true, fmt.Sprintf("Model: %s", arg)
	}
	return false, ""
}

// ParseCommandLocal splits "/name rest" without touching pkg/tools (kept
// local so TUI-only commands stay invisible to the shared registry).
func ParseCommandLocal(line string) (name, raw string, ok bool) {
	if !strings.HasPrefix(line, "/") {
		return "", "", false
	}
	rest := line[1:]
	sp := strings.IndexAny(rest, " \t")
	if sp < 0 {
		return rest, "", rest != ""
	}
	return rest[:sp], rest[sp+1:], true
}

func unknownCommandText(name string) string {
	return fmt.Sprintf("Unknown command: /%s", name)
}

// mergedCommandDefs unions the shared registry with TUI-local commands for
// completion and help. Registry entries win on name collisions.
func mergedCommandDefs(toolReg *tools.ToolRegistry) []commandInfo {
	seen := map[string]bool{}
	out := []commandInfo{
		{Name: "thinking", Description: "Set reasoning effort: /thinking low|high|max|off (无参数循环切换)."},
		{Name: "model", Description: "Show or switch the model: /model [model-id]."},
	}
	for _, d := range out {
		seen[d.Name] = true
	}
	if toolReg != nil && toolReg.Commands != nil {
		for _, d := range toolReg.Commands.List() {
			if seen[d.Name] {
				continue
			}
			seen[d.Name] = true
			out = append(out, commandInfo{Name: d.Name, Description: d.Description})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// startKeyPump owns stdin bytes in raw mode and decodes them into key events.
// A small disambiguation timer resolves lone ESC vs Alt-combos vs CSI
// sequences (see keys.go).
func startKeyPump() <-chan keyEvent {
	ch := make(chan keyEvent, 128)
	go func() {
		defer close(ch)
		reader := bufio.NewReaderSize(os.Stdin, 4096)
		parser := newKeyParser()
		var timer *time.Timer
		var timerC <-chan time.Time
		buf := make([]byte, 1024)
		type readResult struct {
			n   int
			err error
		}
		reads := make(chan readResult, 1)
		go func() {
			n, err := reader.Read(buf)
			reads <- readResult{n: n, err: err}
		}()

		arm := func() {
			if !parser.Pending() {
				if timer != nil {
					timer.Stop()
					timer = nil
				}
				timerC = nil
				return
			}
			d := parser.pendingDeadline()
			if timer == nil {
				timer = time.NewTimer(d)
				timerC = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(d)
			}
		}

		emit := func(evs []keyEvent) {
			for _, ev := range evs {
				ch <- ev
			}
		}

		for {
			select {
			case result := <-reads:
				if result.n > 0 {
					var evs []keyEvent
					for _, b := range buf[:result.n] {
						evs = append(evs, parser.Feed(b)...)
					}
					emit(evs)
					arm()
				}
				if result.err != nil {
					emit(parser.Flush())
					ch <- keyEvent{kind: keyEOF}
					return
				}
				reads = make(chan readResult, 1)
				go func() {
					n, err := reader.Read(buf)
					reads <- readResult{n: n, err: err}
				}()
			case <-timerC:
				timer = nil
				timerC = nil
				emit(parser.Flush())
			}
		}
	}()
	return ch
}

const approvalPromptStr = ColorBold + "? " + ColorReset

const cookedLimitedBanner = "Raw input unavailable; using cooked input (completion and live editing are limited)."

// buildHelpText renders commands plus the keybinding cheat-sheet.
func buildHelpText(defs []commandInfo) string {
	var b strings.Builder
	b.WriteString("\nCommands:\n")
	b.WriteString("  /help     Show this help\n")
	b.WriteString("  /clear    Clear the screen\n")
	b.WriteString("  /exit     Leave TUI (/quit)\n")
	for _, d := range defs {
		if d.Name == "help" || d.Name == "exit" {
			continue
		}
		fmt.Fprintf(&b, "  /%-9s %s\n", d.Name, d.Description)
	}
	b.WriteString("\nKeys:\n")
	b.WriteString("  输入 /     命令补全：↑↓ 选择，Tab/Enter 完成，Esc 关闭\n")
	b.WriteString("  Enter 提交；Ctrl+J / Alt+Enter 插入换行（多行输入）\n")
	b.WriteString("  ↑/↓       输入历史（多行内先移动光标）；Ctrl+P/Ctrl+N 直翻历史\n")
	b.WriteString("  Ctrl+P   命令面板（模糊搜索动作）\n")
	b.WriteString("  Tab       prompt ↔ scrollback 焦点切换\n")
	b.WriteString("  —— scrollback（浏览历史）——\n")
	b.WriteString("  ↑↓/PgUp/PgDn  选择/翻页；Shift←→ 跳 turn\n")
	b.WriteString("  Esc  中断回答(保留草稿)；idle 非空 2xEsc 清空(Ctrl+S 恢复)\n")
	b.WriteString("  Ctrl+C  清行/关弹窗；空输入 2 秒内再按退出\n")
	b.WriteString("  Ctrl+S  草稿 stash/pop\n")
	b.WriteString("  /vim-mode  切换 Vim 导航：j/k 移动、H/L 跳 turn、h/l/e 折叠、E 全部\n")
	b.WriteString("  鼠标    滚轮滚动、左键聚焦 prompt\n")
	b.WriteString("\nApproval(审批卡)：↑↓/Tab 循环选项，1-9/Enter 确认，Ctrl+O 常允许，Esc 暂离(再按取消)\n")
	return b.String()
}

// -----------------------------------------------------------------------
// Legacy cooked-mode fallback: byte-for-byte the pre-overhaul loop, kept for
// piped stdin where raw mode is impossible.

func runCookedTUI(store gateway.SessionStore, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter, modelName string, runtime agent.PluginRuntime, instructions string) bool {
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
	ag.Instructions = instructions
	ag.AttachPluginRuntime(runtime)
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

	if _, _, ok := terminalSize(); ok {
		ui.write(ColorYellow + cookedLimitedBanner + ColorReset + "\n")
	}
	ui.write(banner(modelName, tuiCwd()))
	ui.prompt()

	// Same P1-7 double-strike guard as the raw path (快胜#3). Cooked mode has
	// no status bar, so the warning goes to the output stream instead. During
	// an approval wait the interrupt channel is owned by readApproval, whose
	// Ctrl+C only cancels the approval — this branch cannot fire then.
	var lastCtrlC time.Time // zero = exit window disarmed
	for {
		select {
		case <-interrupt:
			quit, ref := resolveCtrlC(lastCtrlC, time.Now())
			lastCtrlC = ref
			if quit {
				return true
			}
			ui.write(ColorYellow + ctrlCPrompt + ColorReset + "\n")
			ui.prompt()
		case req := <-approvalCh:
			ui.write(formatApproval(req) + promptStr)
			if !readApproval(inputCh, interrupt, ui, req) {
				return true
			}
		case line, ok := <-inputCh:
			if !ok {
				return true
			}
			ui.consumed()
			line = strings.TrimSpace(line)
			if line == "" {
				ui.prompt()
				continue
			}
			if line == "/exit" || line == "/quit" {
				return true
			}
			if line == "/help" {
				ui.write(helpText(toolReg))
				ui.prompt()
				continue
			}
			if line == "/clear" {
				ui.clear()
				ui.write(banner(modelName, tuiCwd()))
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
						return true
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
			if name, _, ok := ParseCommandLocal(line); ok && name != "" {
				ui.write(ColorYellow + unknownCommandText(name) + ColorReset + "\n")
				ui.prompt()
				continue
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

// readApproval waits for an approval decision line. It also listens on the
// interrupt channel so Ctrl+C during the approval wait maps to ApprovalCancel
// instead of deadlocking the user (who would otherwise have to type 'c').
// The cancel settles THIS approval and returns true (= stay in the TUI); the
// main loop resumes and the next approval opens a fresh wait. Returning false
// tells the main loop to exit the TUI (stdin EOF).
func readApproval(inputCh <-chan string, interrupt <-chan os.Signal, ui *UI, req approvalRequest) bool {
	for {
		select {
		case <-interrupt:
			// Ctrl+C cancels the approval only; it must never quit the
			// program (P1-7).
			req.decision <- tools.ApprovalCancel
			ui.write("\n(Cancelled by Ctrl+C — approval cancelled, TUI stays open.)\n" + promptStr)
			return true
		case line, ok := <-inputCh:
			if !ok {
				req.decision <- tools.ApprovalCancel
				return false
			}
			ui.consumed()
			if d, ok := parseApproval(line, req.options); ok {
				req.decision <- d
				return true
			}
			ui.write("  无效选择，请输入 y / n / a / c 或选项编号/id\n" + promptStr)
			// Stay in the wait: returning here stranded RequestUser forever.
		}
	}
}

// helpText keeps the legacy-path signature; both paths share one rendering.
func helpText(toolReg *tools.ToolRegistry) string {
	return buildHelpText(mergedCommandDefs(toolReg))
}

// paletteActions builds the Ctrl+P command-palette action list. Each action
func paletteActions(submit func(string), exit func()) []paletteItem {
	return []paletteItem{
		{Label: "New Session", Detail: "start a fresh conversation", Run: func() { submit("/new") }},
		{Label: "Resume Session", Detail: "pick a previous session", Run: func() { submit("/resume") }},
		{Label: "Plan Mode", Detail: "toggle plan mode", Run: func() { submit("/plan") }},
		{Label: "Permission: accept-edits", Detail: "auto-allow edits, ask on commands", Run: func() { submit("/permission accept-edits") }},
		{Label: "Permission: plan", Detail: "read-only, deny writes", Run: func() { submit("/permission plan") }},
		{Label: "Permission: auto", Detail: "small-model reviews destructive tools", Run: func() { submit("/permission auto") }},
		{Label: "Permission: allow-all", Detail: "auto-allow everything", Run: func() { submit("/permission allow-all") }},
		{Label: "Permission: default", Detail: "ask every time", Run: func() { submit("/permission default") }},
		{Label: "Thinking: cycle", Detail: "off -> low -> high -> max", Run: func() { submit("/thinking") }},
		{Label: "Clear Screen", Detail: "clear the transcript", Run: func() { submit("/clear") }},
		{Label: "Help", Detail: "commands and key bindings", Run: func() { submit("/help") }},
		{Label: "Exit TUI", Detail: "quit", Run: exit},
	}
}

// onOff renders a boolean as "on"/"off" for status messages.
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

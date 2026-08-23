package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// TerminalSession is one persistent terminal process owned by a session
// (upstream dsh-terminal + tool-terminal contract). Transport is a pipe pair
// (no PTY dependency, matching the persistent-shell decision): input goes to
// stdin, output is retained in a bounded ring of lines that terminal_read
// pages, and command completion is detected with a sentinel marker so
// terminal_send can wait for output silence without a real tty.
type TerminalSession struct {
	ID         string
	Owner      string // session id fence (authorization boundary)
	Name       string
	Type       string
	Cwd        string
	Pid        int
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	reader     *bufio.Reader
	mu         sync.Mutex
	lines      []string
	maxLines   int
	seq        int // send sequence, also marker counter
	closed     bool
	waiters    map[string]chan struct{} // marker -> completion
	background map[string]*Job          // marker -> pty-send job
}

var (
	terminalMu       sync.Mutex
	terminals        = map[string]*TerminalSession{}
	terminalSeq      = 0
	terminalMaxLines = 2000
)

// newTerminalSession spawns the shell process and starts the retained-output
// reader goroutine. The shell dialect follows the host platform.
func newTerminalSession(owner, termType, name, cwd string) (*TerminalSession, error) {
	if termType != "" && termType != "shell" {
		return nil, fmt.Errorf("unknown terminal backend type %q (supported: shell)", termType)
	}
	terminalMu.Lock()
	terminalSeq++
	id := fmt.Sprintf("term-%d", terminalSeq)
	terminalMu.Unlock()

	shellName := "powershell"
	shellArgs := []string{"-NoProfile", "-NonInteractive", "-Command", "-"}
	if runtime.GOOS != "windows" {
		shellName = "bash"
		shellArgs = []string{"--norc", "--noprofile", "-s"}
	}
	cmd := exec.Command(shellName, shellArgs...)
	makeProcessGroup(cmd)
	if cwd != "" {
		cmd.Dir = cwd
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Use an io.Pipe pair exactly like the persistent-shell transport:
	// StdoutPipe() combined with a working directory stalls PowerShell
	// stdin-mode output on Windows, while the pipe-pair path (which the
	// long-lived shell tools already use) drains reliably.
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	attachProcessGroup(cmd)
	go func() {
		_ = cmd.Wait()
		releaseProcessGroup(cmd)
		_ = pw.Close()
	}()
	term := &TerminalSession{
		ID:         id,
		Owner:      owner,
		Name:       name,
		Type:       termType,
		Cwd:        cwd,
		Pid:        cmd.Process.Pid,
		cmd:        cmd,
		stdin:      stdin,
		reader:     bufio.NewReader(pr),
		maxLines:   terminalMaxLines,
		waiters:    map[string]chan struct{}{},
		background: map[string]*Job{},
	}
	terminalMu.Lock()
	terminals[id] = term
	terminalMu.Unlock()

	go term.readLoop()
	return term, nil
}

// readLoop drains the shell stdout forever, retaining the bounded tail.
func (t *TerminalSession) readLoop() {
	for {
		line, err := t.reader.ReadString('\n')
		if err != nil {
			t.mu.Lock()
			t.closed = true
			waiters := t.waiters
			t.waiters = map[string]chan struct{}{}
			background := t.background
			t.background = map[string]*Job{}
			t.mu.Unlock()
			for _, done := range waiters {
				close(done)
			}
			for _, job := range background {
				job.mu.Lock()
				if job.Status == JobRunning {
					job.Status = JobFailed
					job.Detail = "terminal session exited"
				}
				job.mu.Unlock()
				close(job.done)
			}
			return
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		t.mu.Lock()
		t.appendLineLocked(line)
		// Wake matching waiters when the sentinel line lands.
		for marker, done := range t.waiters {
			if line == marker {
				delete(t.waiters, marker)
				close(done)
			}
		}
		for marker, job := range t.background {
			if line == marker {
				delete(t.background, marker)
				job.mu.Lock()
				if job.Status == JobRunning {
					job.Status = JobCompleted
					job.Detail = "exit code: 0"
				}
				job.mu.Unlock()
				close(job.done)
			}
		}
		t.mu.Unlock()
	}
}

// appendLineLocked keeps the bounded ring of retained output.
func (t *TerminalSession) appendLineLocked(line string) {
	t.lines = append(t.lines, line)
	if len(t.lines) > t.maxLines {
		drop := len(t.lines) - t.maxLines
		t.lines = append([]string(nil), t.lines[drop:]...)
	}
}

// send writes text and optionally waits for the command to settle (marker).
// Returns the newly produced output text.
func (t *TerminalSession) send(ctx context.Context, text string, submit bool, backgroundJob *Job) (string, error) {
	payload := text
	if submit {
		payload += "\n"
	}
	if _, err := t.stdin.Write([]byte(payload)); err != nil {
		return "", fmt.Errorf("terminal send: %w", err)
	}
	if backgroundJob != nil {
		// Register the marker BEFORE writing it: the reader goroutine may
		// drain the echoed marker line the moment it lands, so the waiter
		// must already be in place (register-then-write ordering).
		marker := t.marker()
		t.mu.Lock()
		t.background[marker] = backgroundJob
		t.mu.Unlock()
		line := "echo " + marker + "\n"
		if _, err := t.stdin.Write([]byte(line)); err != nil {
			return "", err
		}
		return "", nil
	}
	marker := t.marker()
	done := make(chan struct{})
	t.mu.Lock()
	before := len(t.lines)
	t.waiters[marker] = done
	t.mu.Unlock()
	line := "echo " + marker + "\n"
	if _, err := t.stdin.Write([]byte(line)); err != nil {
		t.mu.Lock()
		delete(t.waiters, marker)
		t.mu.Unlock()
		return "", err
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.mu.Lock()
		delete(t.waiters, marker)
		t.mu.Unlock()
		return "", ctx.Err()
	case <-time.After(120 * time.Second):
		t.mu.Lock()
		delete(t.waiters, marker)
		t.mu.Unlock()
	}
	t.mu.Lock()
	after := len(t.lines)
	out := strings.Join(t.lines[before:after], "\n")
	// Remove the marker line from retained output so reads stay clean.
	t.removeLineLocked(marker)
	t.mu.Unlock()
	return out, nil
}

func (t *TerminalSession) marker() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	return fmt.Sprintf("__DSH_TERM_%d_%d__", t.Pid, t.seq)
}

func (t *TerminalSession) removeLineLocked(target string) {
	out := t.lines[:0]
	for _, l := range t.lines {
		if l != target {
			out = append(out, l)
		}
	}
	t.lines = out
}

// readPage returns a page of retained output: count lines ending at the
// newest-relative offset.
func (t *TerminalSession) readPage(offset, count int) (text string, total, begin, end int, truncated bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	total = len(t.lines)
	if count <= 0 {
		count = 500
	}
	end = total - offset
	if end < 0 {
		end = 0
	}
	begin = end - count
	if begin < 0 {
		begin = 0
	}
	if begin >= total {
		return "", total, 0, 0, false
	}
	truncated = total > end
	return strings.Join(t.lines[begin:end], "\n"), total, begin, end, truncated
}

// closeTerminal stops the process and unregisters the session.
func (t *TerminalSession) close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.mu.Unlock()
	_ = t.stdin.Close()
	if t.cmd != nil {
		_ = killProcessTree(t.cmd)
	}
	_, _ = t.cmd.Process.Wait()
	terminalMu.Lock()
	delete(terminals, t.ID)
	terminalMu.Unlock()
	return nil
}

func lookupTerminal(owner, id string) (*TerminalSession, bool) {
	terminalMu.Lock()
	defer terminalMu.Unlock()
	t, ok := terminals[id]
	if !ok || t.Owner != owner {
		return nil, false
	}
	return t, true
}

// terminalSignal maps a requested signal name onto the platform kill path.
func (t *TerminalSession) signal(name string) error {
	sig, err := terminalSignal(name)
	if err != nil {
		return err
	}
	if t.cmd.Process == nil {
		return fmt.Errorf("terminal process not running")
	}
	if runtime.GOOS == "windows" {
		// Windows has no portable signal delivery to a console process group;
		// terminate the process tree the same way job_kill does.
		return killProcessTree(t.cmd)
	}
	return t.cmd.Process.Signal(sig)
}

// RegisterTerminalTools registers terminal_open, terminal_send, terminal_read,
// terminal_list, terminal_signal, terminal_close (upstream dsh-tool-terminal
// contract over the pipe transport).
func (r *ToolRegistry) RegisterTerminalTools() {
	r.Register(ToolDefinition{
		Name:        "terminal_open",
		Description: "Create a persistent, owner-isolated terminal session of a registered backend type. Use this for shell state that must survive across tool calls.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"type": { "type": "string", "description": "Registered terminal backend type, usually shell" },
				"name": { "type": "string", "description": "Optional owner-local display name" },
				"cwd": { "type": "string", "description": "Initial working directory" }
			},
			"required": ["type"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Type string `json:"type"`
				Name string `json:"name"`
				Cwd  string `json:"cwd"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			cwd := ctx.Cwd
			if args.Cwd != "" {
				cwd = args.Cwd
			}
			term, err := newTerminalSession(ctx.SessionID, args.Type, args.Name, cwd)
			if err != nil {
				return nil, err
			}
			return fmt.Sprintf("sessionId: %s\npid: %d\nname: %s\ntype: %s", term.ID, term.Pid, term.Name, term.Type), nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "terminal_send",
		Description: "Send text to a persistent terminal. By default Enter is submitted and the call waits for the command to settle (output silence, timeout or session exit).",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"sessionId": { "type": "string", "description": "Terminal session id returned by terminal_open or terminal_list" },
				"text": { "type": "string", "description": "UTF-8 text to write to the terminal" },
				"submit": { "type": "boolean", "description": "Submit Enter after text (default true)" }
			},
			"required": ["sessionId", "text"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				SessionID string `json:"sessionId"`
				Text      string `json:"text"`
				Submit    *bool  `json:"submit"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			term, ok := lookupTerminal(ctx.SessionID, args.SessionID)
			if !ok {
				return nil, fmt.Errorf("unknown terminal session %q", args.SessionID)
			}
			submit := true
			if args.Submit != nil {
				submit = *args.Submit
			}
			out, err := term.send(ctx.Context, args.Text, submit, nil)
			if err != nil {
				return nil, err
			}
			return fmt.Sprintf("kind: foreground\n%s", out), nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "terminal_read",
		Description: "Read a bounded page of retained output from a persistent terminal without sending input.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"sessionId": { "type": "string" },
				"offset": { "type": "integer", "description": "Newest-relative line offset (default 0)" },
				"count": { "type": "integer", "description": "Requested line count (default 500)" }
			},
			"required": ["sessionId"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				SessionID string `json:"sessionId"`
				Offset    int    `json:"offset"`
				Count     int    `json:"count"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			term, ok := lookupTerminal(ctx.SessionID, args.SessionID)
			if !ok {
				return nil, fmt.Errorf("unknown terminal session %q", args.SessionID)
			}
			text, total, begin, end, truncated := term.readPage(args.Offset, args.Count)
			return fmt.Sprintf("text: %s\ntotalLines: %d\nlineBegin: %d\nlineEnd: %d\ntruncated: %v", text, total, begin, end, truncated), nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "terminal_list",
		Description: "List persistent terminal sessions owned by this session.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {},
			"required": []
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			terminalMu.Lock()
			defer terminalMu.Unlock()
			var out []string
			for _, t := range terminals {
				if t.Owner != ctx.SessionID {
					continue
				}
				out = append(out, fmt.Sprintf("sessionId: %s\npid: %d\nname: %s\ntype: %s", t.ID, t.Pid, t.Name, t.Type))
			}
			if len(out) == 0 {
				return "No terminal sessions.", nil
			}
			return strings.Join(out, "\n---\n"), nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "terminal_signal",
		Description: "Send an allowed signal (SIGINT, SIGTERM, SIGKILL, SIGTSTP) to the terminal process. On Windows all signals map to process termination.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"sessionId": { "type": "string" },
				"signal": { "type": "string", "enum": ["SIGINT", "SIGTERM", "SIGKILL", "SIGTSTP"] }
			},
			"required": ["sessionId", "signal"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				SessionID string `json:"sessionId"`
				Signal    string `json:"signal"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			term, ok := lookupTerminal(ctx.SessionID, args.SessionID)
			if !ok {
				return nil, fmt.Errorf("unknown terminal session %q", args.SessionID)
			}
			if err := term.signal(args.Signal); err != nil {
				return nil, err
			}
			return fmt.Sprintf("Sent %s to terminal %s.", args.Signal, args.SessionID), nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "terminal_close",
		Description: "Close a persistent terminal session, discarding its retained output.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"sessionId": { "type": "string" }
			},
			"required": ["sessionId"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				SessionID string `json:"sessionId"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			term, ok := lookupTerminal(ctx.SessionID, args.SessionID)
			if !ok {
				return nil, fmt.Errorf("unknown terminal session %q", args.SessionID)
			}
			if err := term.close(); err != nil {
				return nil, err
			}
			return fmt.Sprintf("Closed terminal %s.", args.SessionID), nil
		},
	})
}

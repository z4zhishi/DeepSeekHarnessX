package tools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// PersistentShell holds one long-lived shell process whose working directory
// and exported environment survive across tool calls (upstream
// tool-bash-persistent semantics over a pipe transport, no PTY dependency).
type PersistentShell struct {
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	stdout       io.ReadCloser
	stdoutReader *bufio.Reader
	endMarker    string
	closed       bool
}

var (
	shellMu  sync.Mutex
	shells   = map[string]*PersistentShell{} // keyed by session id
	shellSeq = 0
)

// RegisterPersistentShellTools registers bash_persistent and bash_reset.
func (r *ToolRegistry) RegisterPersistentShellTools() {
	r.Register(ToolDefinition{
		Name:         "bash_persistent",
		Description:  "Run a command in a persistent shell. Working directory and environment state persist across calls for this session.",
		RequiresPerm: true,
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"command": { "type": "string", "description": "Shell command line to execute" },
				"timeout_ms": { "type": "integer", "description": "Optional timeout in ms (default 60000)" }
			},
			"required": ["command"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Command   string `json:"command"`
				TimeoutMs int    `json:"timeout_ms"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			if args.Command == "" {
				return nil, fmt.Errorf("command must be a non-empty string")
			}
			shell := getShell(ctx.SessionID)
			if shell == nil {
				return nil, fmt.Errorf("failed to start persistent shell")
			}
			marker := shell.endMarker
			line := args.Command + "\n" + "echo " + marker + "\n"
			if _, err := shell.stdin.Write([]byte(line)); err != nil {
				return nil, fmt.Errorf("persistent shell write: %w", err)
			}
			var out strings.Builder
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					chunk, err := shell.stdoutReader.ReadString('\n')
					if err != nil {
						break
					}
					if strings.Contains(chunk, marker) {
						break
					}
					out.WriteString(chunk)
				}
			}()
			select {
			case <-done:
			case <-ctx.Context.Done():
				// Reset the shell: killing the process EOFs the pipe, which
				// unblocks the reader goroutine and prevents it from stealing
				// the next command's output. The next call starts fresh.
				closeShell(ctx.SessionID)
				return nil, fmt.Errorf("persistent shell: %w", ctx.Context.Err())
			}
			return out.String(), nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "bash_reset",
		Description: "Reset the persistent shell for this session, discarding its working directory and environment state.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"reason": { "type": "string", "description": "Optional reset reason" }
			},
			"required": []
		}`),
		Execute: func(ctx ToolExecutionContext, args string) (any, error) {
			closeShell(ctx.SessionID)
			return "Persistent shell reset.", nil
		},
	})
}

// getShell returns the session's persistent shell, starting one on first use.
func getShell(sessionID string) *PersistentShell {
	shellMu.Lock()
	defer shellMu.Unlock()
	if s, ok := shells[sessionID]; ok && !s.closed {
		return s
	}
	shellSeq++
	marker := fmt.Sprintf("__DSH_END_%d__", shellSeq)
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", "-")
	} else {
		cmd = exec.Command("bash", "--norc", "--noprofile", "-s")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil
	}
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		return nil
	}
	go func() {
		_ = cmd.Wait()
		_ = pw.Close()
	}()
	shell := &PersistentShell{
		cmd:          cmd,
		stdin:        stdin,
		stdout:       pr,
		stdoutReader: bufio.NewReader(pr),
		endMarker:    marker,
	}
	shells[sessionID] = shell
	return shell
}

// closeShell stops and removes one session's shell process.
func closeShell(sessionID string) {
	shellMu.Lock()
	defer shellMu.Unlock()
	s, ok := shells[sessionID]
	if !ok {
		return
	}
	s.closed = true
	_ = s.cmd.Process.Kill()
	delete(shells, sessionID)
}

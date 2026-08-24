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
// tool-bash-persistent / tool-pwsh-persistent semantics over a pipe transport,
// no PTY dependency).
//
// Dialects are parameterized: "bash" and "pwsh". On Windows pwsh resolves to
// the system powershell.exe; elsewhere it must be an actual pwsh on PATH.
type PersistentShell struct {
	dialect      string
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	stdout       io.ReadCloser
	stdoutReader *bufio.Reader
	endMarker    string
	closed       bool
}

// shellDialect names a persistent-shell transport dialect.
type shellDialect string

const (
	dialectBash shellDialect = "bash"
	dialectPwsh shellDialect = "pwsh"
)

var (
	shellMu  sync.Mutex
	shells   = map[string]*PersistentShell{} // keyed by dialect + ":" + session id
	shellSeq = 0
)

func shellKey(d shellDialect, sessionID string) string {
	return string(d) + "|" + sessionID
}

// RegisterPersistentShellTools registers bash_persistent and bash_reset.
func (r *ToolRegistry) RegisterPersistentShellTools() {
	r.registerPersistentShellFamily(dialectBash, "bash_persistent", "bash_reset", bashBinaryName(),
		"Run a command in a persistent shell. Working directory and environment state persist across calls for this session.",
		"Reset the persistent shell for this session, discarding its working directory and environment state.",
	)
}

// bashBinaryName resolves the transport binary for the bash dialect. On
// Windows the long-lived bash shell is backed by PowerShell (the original
// behavior); elsewhere it is the system `bash`.
func bashBinaryName() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return "bash"
}

// RegisterPwshTools registers pwsh_persistent and pwsh_reset (upstream
// tool-pwsh-persistent). Windows uses powershell.exe; non-Windows uses the
// `pwsh` binary on PATH.
func (r *ToolRegistry) RegisterPwshTools() {
	r.registerPersistentShellFamily(dialectPwsh, "pwsh_persistent", "pwsh_reset", pwshBinaryName(),
		"Run a command in a persistent PowerShell shell. State, including the current directory and exported environment variables, persists across calls for this agent.",
		"Reset the persistent pwsh shell for this session, discarding its working directory and environment state.",
	)
}

func pwshBinaryName() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return "pwsh"
}

// registerPersistentShellFamily wires a <name>_persistent / <name>_reset pair
// over one shell dialect with a shared execution core.
func (r *ToolRegistry) registerPersistentShellFamily(dialect shellDialect, runName, resetName, binary, runDesc, resetDesc string) {
	r.Register(ToolDefinition{
		Name:         runName,
		Description:  runDesc,
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
			return runPersistentShell(ctx, dialect, binary, args.Command)
		},
	})

	r.Register(ToolDefinition{
		Name:        resetName,
		Description: resetDesc,
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"reason": { "type": "string", "description": "Optional reset reason" }
			},
			"required": []
		}`),
		Execute: func(ctx ToolExecutionContext, args string) (any, error) {
			closeShellOf(dialect, ctx.SessionID)
			return "Persistent shell reset.", nil
		},
	})
}

// runPersistentShell writes one command to the session's dialect shell and
// blocks until its completion marker echoes back.
func runPersistentShell(ctx ToolExecutionContext, dialect shellDialect, binary, command string) (string, error) {
	shell := getShell(dialect, binary, ctx.SessionID)
	if shell == nil {
		return "", fmt.Errorf("failed to start persistent %s shell", dialect)
	}
	marker := shell.endMarker
	line := command + "\n" + "echo " + marker + "\n"
	if _, err := shell.stdin.Write([]byte(line)); err != nil {
		return "", fmt.Errorf("persistent shell write: %w", err)
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
		closeShellOf(dialect, ctx.SessionID)
		return "", fmt.Errorf("persistent shell: %w", ctx.Context.Err())
	}
	return out.String(), nil
}

// getShell returns the session's persistent shell for one dialect, starting
// one on first use. Returns nil if the shell binary is unavailable or spawn
// fails (the caller reports the error).
func getShell(dialect shellDialect, binary, sessionID string) *PersistentShell {
	key := shellKey(dialect, sessionID)
	shellMu.Lock()
	defer shellMu.Unlock()
	if s, ok := shells[key]; ok && !s.closed {
		return s
	}
	shellSeq++
	marker := fmt.Sprintf("__DSH_END_%d__", shellSeq)
	var cmd *exec.Cmd
	if dialect == dialectPwsh || (dialect == dialectBash && runtime.GOOS == "windows") {
		// pwsh and the Windows bash fallback both drive a PowerShell transport.
		cmd = exec.Command(binary, "-NoProfile", "-NonInteractive", "-Command", "-")
	} else {
		cmd = exec.Command(binary, "--norc", "--noprofile", "-s")
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
		dialect:      string(dialect),
		cmd:          cmd,
		stdin:        stdin,
		stdout:       pr,
		stdoutReader: bufio.NewReader(pr),
		endMarker:    marker,
	}
	shells[key] = shell
	return shell
}

// closeShellOf stops and removes one session's shell process for a dialect.
func closeShellOf(dialect shellDialect, sessionID string) {
	shellMu.Lock()
	defer shellMu.Unlock()
	key := shellKey(dialect, sessionID)
	s, ok := shells[key]
	if !ok {
		return
	}
	s.closed = true
	_ = s.cmd.Process.Kill()
	delete(shells, key)
}

// closeShell stops and removes one session's bash shell process.
func closeShell(sessionID string) {
	closeShellOf(dialectBash, sessionID)
}

//go:build windows

package tools

import (
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
)

// makeProcessGroup gives cmd its own console process group so a tree kill via
// taskkill /T can enumerate every descendant (a shell/interpreted command's
// grandchildren would otherwise reparent and survive a bare PID kill).
func makeProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killProcessTree forcibly terminates a process and its whole descendant tree.
// A bare Process.Kill() only terminates the root PID: shells/interpreters
// (powershell, node, bash) spawn grandchildren that survive the kill, keep
// running, and hold the stdout/stderr pipe handle open — pinning the working
// directory and making RemoveAll fail with a Windows sharing violation.
// taskkill /T walks the tree so the pipes EOF and the runner goroutine reaps.
func killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	return exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}

// terminalSignal validates a requested signal name on Windows. Windows has no
// portable delivery of SIGINT/SIGTSTP to a console process group; the caller
// maps every accepted name onto process termination.
func terminalSignal(name string) (syscall.Signal, error) {
	switch name {
	case "SIGINT", "SIGTERM", "SIGKILL", "SIGTSTP":
		return syscall.Signal(0), nil
	default:
		return 0, fmt.Errorf("unsupported signal %q", name)
	}
}

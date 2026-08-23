//go:build !windows

package tools

import (
	"fmt"
	"os/exec"
	"syscall"
)

// makeProcessGroup gives cmd its own process group (Setpgid) so the root PID
// becomes its group leader and killProcessTree can reach every descendant via
// the negative pgid. Without this, a child-spawning command would orphan its
// grandchildren on kill and leak their pipe handles.
func makeProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree terminates a process and its whole process group. The job
// commands are launched with Setpgid=true (see jobs.go), so the root PID is
// its own group leader and signalling the negative pgid reaches every child.
// This mirrors the Windows taskkill /T tree-kill, preventing orphaned
// grandchildren from holding the stdout/stderr pipe handles open.
func killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// terminalSignal resolves a requested signal name to a platform signal on
// Unix-like systems. The enumeration is fixed by the tool schema.
func terminalSignal(name string) (syscall.Signal, error) {
	switch name {
	case "SIGINT":
		return syscall.SIGINT, nil
	case "SIGTERM":
		return syscall.SIGTERM, nil
	case "SIGKILL":
		return syscall.SIGKILL, nil
	case "SIGTSTP":
		return syscall.SIGTSTP, nil
	default:
		return 0, fmt.Errorf("unsupported signal %q", name)
	}
}

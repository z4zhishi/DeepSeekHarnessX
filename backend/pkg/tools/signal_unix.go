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

// attachProcessGroup is a no-op on Unix: unlike the Windows Job Object, Unix
// has no post-start group-assignment step — Setpgid in SysProcAttr already
// places the child in its own process group at spawn time (verified by
// assertPshellProcessGroup in pshell_kill_unix_test.go), so by the time
// attachProcessGroup is invoked after Start the association already exists.
// The no-op exists only to satisfy the shared call sites (jobs.go, pshell.go,
// terminal.go) on this GOOS.
func attachProcessGroup(_ *exec.Cmd) {}

// killProcessTree terminates a process and its whole process group. The job
// commands are launched with Setpgid=true (see jobs.go), so the root PID is
// its own group leader and signalling the negative pgid reaches every child.
// This mirrors the Windows Job Object tree-kill, preventing orphaned
// descendants from holding the stdout/stderr pipe handles open.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd == nil {
		return nil
	}
	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	if pid <= 0 {
		return nil
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// releaseProcessGroup is a no-op on Unix: the process group is torn down when
// the group leader dies, so there is no external handle to release.
func releaseProcessGroup(_ *exec.Cmd) {}

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

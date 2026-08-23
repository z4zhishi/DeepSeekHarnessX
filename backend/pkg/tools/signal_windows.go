//go:build windows

package tools

import (
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A process-group association maps an *exec.Cmd to the Windows Job Object that
// owns its whole descendant tree. makeProcessGroup creates the job with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE so that closing the handle (which the
// kill path and the runner-cleanup path both do) forcibly reaps every process
// in the tree — grandchildren included — the same way the OS guarantees an
// orphaned shell's fork cannot survive a kill.
//
// The Windows Job Object is the deterministic fix over taskkill /T: taskkill
// enumerates the console process group from the outside and races the nesting
// shell, so a nested `powershell Start-Sleep` grandchild can slip through and
// keep its working-directory handle (t.TempDir()) pinned, making the test's
// RemoveAll fail with "being used by another process". A Job Object on the
// parent forces every child (and its children, recursively) into the job the
// moment they spawn, and TerminateJobObject kills them all synchronously.
type procGroup struct {
	job windows.Handle
}

var (
	pgMu    sync.Mutex
	pgByCmd = map[*exec.Cmd]*procGroup{}
)

// makeProcessGroup arms cmd to be reaped as a whole tree. On Windows this
// creates a Job Object with KILL_ON_JOB_CLOSE and registers it against cmd.
// It must be called before cmd.Start(); the live-process assignment happens
// right after Start via attachProcessGroup(cmd).
func makeProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		// Fall through to a plain root PID kill; better than nothing.
		return
	}
	// Zeroed struct, only the kill-on-close limit is raised. SetInformationJobObject
	// writes the full struct back; leaving the rest zero means no other limits.
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	pgMu.Lock()
	pgByCmd[cmd] = &procGroup{job: job}
	pgMu.Unlock()
}

// attachProcessGroup associates cmd's already-started process with its Job
// Object, so every descendant (the nested powershell grandchild) is forced
// into the job and reaped by killProcessTree. Must be called once, after
// cmd.Start() succeeds. Assigning an already-exited process is an ignored
// error — a command that finished in milliseconds needs no tree kill.
func attachProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgMu.Lock()
	pg := pgByCmd[cmd]
	pgMu.Unlock()
	if pg == nil {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	_ = windows.AssignProcessToJobObject(pg.job, h)
	_ = windows.CloseHandle(h)
}

// killProcessTree forcibly terminates the whole process tree owned by cmd's
// Job Object. It returns an error only when no tree could be torn down; the
// caller treats it as best-effort.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd == nil {
		return nil
	}
	pgMu.Lock()
	pg := pgByCmd[cmd]
	delete(pgByCmd, cmd)
	pgMu.Unlock()
	if pg == nil {
		// No Job Object (creation failed). Fall back to a root kill + taskkill
		// tree walk so the pipes still EOF.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		if cmd.Process != nil {
			_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		}
		return nil
	}
	if err := windows.TerminateJobObject(pg.job, 1); err != nil {
		// Fall back to the classic tree kill, then still close to trigger
		// kill-on-close for anything that survived.
		_ = windows.CloseHandle(pg.job)
		if cmd.Process != nil {
			_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		}
		return err
	}
	return windows.CloseHandle(pg.job)
}

// releaseProcessGroup closes the job handle after a command has settled
// normally. JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE reaps any stragglers that
// outlived the command (should not normally happen) and frees the handle so
// the process group does not leak across long-lived processes.
func releaseProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	pgMu.Lock()
	pg := pgByCmd[cmd]
	delete(pgByCmd, cmd)
	pgMu.Unlock()
	if pg != nil {
		_ = windows.CloseHandle(pg.job)
	}
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

//go:build windows

package tools

import (
	"fmt"
	"syscall"
)

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

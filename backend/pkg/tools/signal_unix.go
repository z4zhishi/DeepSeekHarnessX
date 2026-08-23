//go:build !windows

package tools

import (
	"fmt"
	"syscall"
)

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

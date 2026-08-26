//go:build !windows && !linux

package tui

// enableRawInput is unavailable on this platform: callers fall back to the
// legacy cooked line-scanner loop (piped/non-tty behavior).
func enableRawInput() (restore func(), ok bool) { return nil, false }

// terminalSize is unavailable without a platform ioctl; callers assume 80x24.
func terminalSize() (width, height int, ok bool) { return 0, 0, false }

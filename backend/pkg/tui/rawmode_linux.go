//go:build linux

package tui

import (
	"os"

	"golang.org/x/sys/unix"
)

// enableRawInput puts stdin into raw mode (no canonical buffering, no echo,
// no signal generation — Ctrl+C arrives as byte 0x03 like on the Windows VT
// path so one key parser serves both). Returns a restore closure; ok=false
// when stdin is not a tty and callers must fall back to line-scanner input.
func enableRawInput() (restore func(), ok bool) {
	fd := int(os.Stdin.Fd())
	saved, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, false
	}
	raw := *saved
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Iflag &^= unix.IXON | unix.ICRNL | unix.BRKINT | unix.INPCK | unix.ISTRIP
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, false
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, saved) }, true
}

// terminalSize reports the tty cell dimensions of stdout.
func terminalSize() (width, height int, ok bool) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, false
	}
	if ws.Col <= 0 || ws.Row <= 0 {
		return 0, 0, false
	}
	return int(ws.Col), int(ws.Row), true
}

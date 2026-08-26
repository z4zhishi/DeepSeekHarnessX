//go:build windows

package tui

import (
	"os"

	"golang.org/x/sys/windows"
)

// Console input mode bits we toggle (mirrored from the Windows SDK; the
// x/sys/windows package exports the same names but keeping the arithmetic
// explicit documents intent).
const (
	wEnableProcessedInput       = 0x0001
	wEnableLineInput            = 0x0002
	wEnableEchoInput            = 0x0004
	wEnableExtendedFlags        = 0x0080
	wEnableQuickEditMode        = 0x0040
	wEnableVirtualTerminalInput = 0x0200
)

const utf8CodePage = 65001

// enableRawInput switches the stdin console into raw VT-input mode: no line
// buffering, no echo, keys delivered as UTF-8/ANSI escape byte streams. It
// also flips both console code pages to UTF-8 so multi-byte CJK input
// survives ReadFile. The returned closure restores everything; ok=false when
// stdin is not a console (piped input) and the caller must fall back to the
// legacy line scanner.
func enableRawInput() (restore func(), ok bool) {
	h := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return nil, false
	}

	cpIn, _ := windows.GetConsoleCP()
	cpOut, _ := windows.GetConsoleOutputCP()

	newMode := mode&^(wEnableProcessedInput|wEnableLineInput|wEnableEchoInput|wEnableQuickEditMode) |
		wEnableVirtualTerminalInput | wEnableExtendedFlags
	if err := windows.SetConsoleMode(h, newMode); err != nil {
		return nil, false
	}
	// Best-effort UTF-8 input decoding; failure keeps single-byte input usable.
	_ = windows.SetConsoleCP(utf8CodePage)
	_ = windows.SetConsoleOutputCP(utf8CodePage)

	return func() {
		_ = windows.SetConsoleMode(h, mode)
		if cpIn != 0 {
			_ = windows.SetConsoleCP(cpIn)
		}
		if cpOut != 0 {
			_ = windows.SetConsoleOutputCP(cpOut)
		}
	}, true
}

// terminalSize reports the visible console window dimensions in cells.
func terminalSize() (width, height int, ok bool) {
	var csbi windows.ConsoleScreenBufferInfo
	h := windows.Handle(os.Stdout.Fd())
	if err := windows.GetConsoleScreenBufferInfo(h, &csbi); err != nil {
		return 0, 0, false
	}
	w := int(csbi.Window.Right-csbi.Window.Left) + 1
	hgt := int(csbi.Window.Bottom-csbi.Window.Top) + 1
	if w <= 0 || hgt <= 0 {
		return 0, 0, false
	}
	return w, hgt, true
}

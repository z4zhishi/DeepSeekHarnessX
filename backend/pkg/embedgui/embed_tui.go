//go:build tui_only

package embedgui

// This file is the pure-TUI ("only TUI") build. `tui_only` strips the Godot
// GUI runtime: the embedded dsh.pck + godot_runner.exe assets and the whole
// launch path are compiled out, so the resulting binary is just the Go
// backend + TUI (tens of MB instead of ~190MB). Any GUI launch returns a clear
// error telling the caller to run the full build or use the TUI directly.

import (
	"errors"
	"io/fs"

	"dsh-go/pkg/gateway"
	"dsh-go/pkg/llm"
	"dsh-go/pkg/tools"
)

// ErrGUIUnavailable is returned by the GUI entry points under `tui_only`.
var ErrGUIUnavailable = errors.New("this is an only-tui build; no Godot GUI is embedded. Use the full build (dshx.exe) or run the TUI mode")

// EmbeddedAssets is the empty filesystem under `tui_only`: no dsh.pck or
// godot_runner is embedded, so nothing is extracted or launched.
var EmbeddedAssets fs.FS = emptyFS{}

type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

// EnsureExtracted is a no-op stub under `tui_only`; it reports the unavailable
// error so any code path that tries to launch the GUI fails loudly instead of
// silently continuing with no assets.
func EnsureExtracted() (string, string, error) {
	return "", "", ErrGUIUnavailable
}

// LaunchAllInOneGUI is the `tui_only` stub for the full-GUI entry point.
func LaunchAllInOneGUI(host string, port int, store gateway.SessionStore, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter) error {
	return ErrGUIUnavailable
}

// LaunchAllInOneGUIWithServer is the `tui_only` stub for the already-wired
// server entry point.
func LaunchAllInOneGUIWithServer(host string, port int, srv *gateway.Server) error {
	return ErrGUIUnavailable
}

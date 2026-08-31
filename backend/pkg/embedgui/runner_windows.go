//go:build windows && !tui_only

package embedgui

import (
	"embed"
	"io/fs"
)

//go:embed assets/godot_runner.exe
var embeddedRunnerFS embed.FS

// readRunnerAsset reads the embedded Godot runner for this GOOS. The Windows
// build embeds the Windows PE runner (assets/godot_runner.exe) so the single
// dshx.exe self-launches the GUI with no system godot required. Non-Windows
// builds must NOT carry a runner: a PE baked into a Linux binary is unusable
// dead weight and extracting it under a Linux name would produce exec format
// errors — those builds report fs.ErrNotExist here and probe the system godot
// instead (see findSystemGodot, runner_other.go, and W7-a).
func readRunnerAsset(name string) ([]byte, error) {
	return embeddedRunnerFS.ReadFile(name)
}

// runnerEmbeddedSize reports the size of the embedded runner without reading
// its bytes; used by the persisted embedded-hash record's fingerprint (see
// embeddedCachedHash / embeddedStamp).
func runnerEmbeddedSize() (int64, bool) {
	fi, err := fs.Stat(embeddedRunnerFS, runnerAsset)
	if err != nil {
		return 0, false
	}
	return fi.Size(), true
}
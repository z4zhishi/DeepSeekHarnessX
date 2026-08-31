//go:build !windows && !tui_only

package embedgui

import "io/fs"

// runnerEmbeddedSize reports no runner on non-Windows builds (W7-a): with no
// runner embedded there is no size to fingerprint (see embeddedCachedHash).
func runnerEmbeddedSize() (int64, bool) { return 0, false }

// Non-Windows builds deliberately embed NO Godot runner (W7-a): the only
// packaged runner asset is a Windows PE, which cannot be executed on this
// GOOS (exec format error). GUI launches on these platforms resolve a runner
// via DSH_GODOT or a system godot on PATH (findSystemGodot) — the embedded
// PCK is still shipped and usable with that runner. The fs.ErrNotExist
// sentinel makes EnsureExtracted skip runner extraction cleanly instead of
// guessing at runtime.
func readRunnerAsset(string) ([]byte, error) {
	return nil, fs.ErrNotExist
}
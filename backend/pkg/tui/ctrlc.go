package tui

import (
	"os"
	"sync"
	"time"
)

// tuiCwd returns the process working directory for the banner; on failure it
// degrades to the session header's "." placeholder so the line never lies
// about a path it could not resolve.
func tuiCwd() string {
	if wd, err := os.Getwd(); err == nil && wd != "" {
		return wd
	}
	return "."
}

// ctrlCExitWindow is how long after a first Ctrl+C a second press still quits
// (ux-benchmark P1-7 / 快胜#3). Generous enough for a human double-tap, short
// enough that the warning never outlives its usefulness.
const ctrlCExitWindow = 2 * time.Second

// ctrlCPrompt is the status-bar hint shown while the exit window is armed.
const ctrlCPrompt = "(再按一次 Ctrl+C 退出)"

// resolveCtrlC decides what one Ctrl+C press means given when the previous
// press landed. It is the pure decision core of the double-strike guard so it
// stays unit-testable without a live terminal:
//
//   - no prior press (or a stale one outside the window): arm — clear the
//     input line / close popups and warn, do NOT quit;
//   - prior press inside the window: quit.
//
// The returned time.Time is the new reference instant to store as "last
// Ctrl+C" (zero value when the window should be considered disarmed, i.e.
// after a quit).
func resolveCtrlC(last time.Time, now time.Time) (quit bool, nextRef time.Time) {
	if !last.IsZero() && now.Sub(last) <= ctrlCExitWindow {
		return true, time.Time{}
	}
	return false, now
}

// ctrlCArmed reports whether a stored reference instant still represents an
// armed exit window at time now.
func ctrlCArmed(last, now time.Time) bool {
	return !last.IsZero() && now.Sub(last) <= ctrlCExitWindow
}

// hintBar holds the transient status-bar hint (e.g. the Ctrl+C warning).
// Written from the main loop and the expiry goroutine, read from the screen
// snapshot goroutine — hence the mutex. A generation counter makes expiry
// idempotent: only the goroutine that armed the CURRENT text may clear it, so
// overlapping windows never fight.
type hintBar struct {
	mu   sync.Mutex
	text string
	gen  uint64
}

// set installs text and returns its generation (used to arm lazy expiry).
func (h *hintBar) set(text string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gen++
	h.text = text
	return h.gen
}

// clearIfGen clears the hint only when gen is still the live generation.
func (h *hintBar) clearIfGen(gen uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.text != "" && h.gen == gen {
		h.text = ""
		return true
	}
	return false
}

// get snapshots the current hint text for rendering.
func (h *hintBar) get() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.text
}

package tui

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// History is a bounded input-history ring with readline-style navigation.
// Navigation keeps a draft (the text being typed before the user dipped into
// history) so pressing Down past the newest entry restores it — the behavior
// every mainstream shell and Claude Code-style prompt provides.
type History struct {
	mu      sync.Mutex
	items   []string // oldest first
	cap     int
	navPos  int    // index into items while navigating; == len(items) when not navigating
	navigat bool   // true between the first Up and the return to the live draft
	draft   string // pre-navigation buffer content
}

// NewHistory creates an empty history holding at most cap entries.
func NewHistory(cap int) *History {
	if cap <= 0 {
		cap = 200
	}
	return &History{cap: cap}
}

// Add appends one submitted line. Consecutive duplicates collapse, and an
// exact repeat of the newest entry is dropped; both keep rapid resubmits from
// polluting navigation. Any in-flight navigation resets to the live prompt.
func (h *History) Add(s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return
	}
	if n := len(h.items); n > 0 && h.items[n-1] == s {
		return
	}
	h.items = append(h.items, s)
	if len(h.items) > h.cap {
		h.items = h.items[len(h.items)-h.cap:]
	}
	h.navigat = false
	h.draft = ""
}

// Len reports the number of stored entries.
func (h *History) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.items)
}

// Prev moves one entry toward the past, returning it. The first call stashes
// the current buffer as the draft. Returns ok=false when already at the
// oldest entry.
func (h *History) Prev(current string) (entry string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.items) == 0 {
		return "", false
	}
	if !h.navigat {
		h.navigat = true
		h.draft = current
		h.navPos = len(h.items)
	}
	if h.navPos == 0 {
		return "", false
	}
	h.navPos--
	return h.items[h.navPos], true
}

// Next moves one entry toward the present. Moving past the newest entry ends
// navigation and restores the stashed draft.
func (h *History) Next() (entry string, end bool, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.navigat {
		return "", false, false
	}
	if h.navPos >= len(h.items)-1 {
		h.navigat = false
		h.navPos = len(h.items)
		d := h.draft
		h.draft = ""
		return d, true, true
	}
	h.navPos++
	return h.items[h.navPos], false, true
}

// ResetNav cancels any in-flight navigation without touching the draft.
func (h *History) ResetNav() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.navigat = false
	h.navPos = len(h.items)
	h.draft = ""
}

// tuiHistorySubpath is joined under os.UserConfigDir().
var tuiHistorySubpath = filepath.Join("dshx", "tui-history.txt")

// DefaultHistoryPath resolves the per-user persistence path:
// os.UserConfigDir()/dshx/tui-history.txt (e.g.
// %AppData%\dshx\tui-history.txt on Windows). ok=false when the OS cannot
// provide a config dir — callers then simply skip persistence.
func DefaultHistoryPath() (path string, ok bool) {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return "", false
	}
	return filepath.Join(dir, tuiHistorySubpath), true
}

// LoadFile reads newline-separated history entries from path (missing file is
// not an error). Blank lines are skipped; capacity applies after load.
func (h *History) LoadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		h.items = append(h.items, line)
	}
	if len(h.items) > h.cap {
		h.items = h.items[len(h.items)-h.cap:]
	}
	return nil
}

// SaveFile atomically writes all entries to path (tmp+rename), creating parent
// directories as needed.
func (h *History) SaveFile(path string) error {
	h.mu.Lock()
	snapshot := make([]string, len(h.items))
	copy(snapshot, h.items)
	h.mu.Unlock()
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	var b strings.Builder
	for _, item := range snapshot {
		b.WriteString(item)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

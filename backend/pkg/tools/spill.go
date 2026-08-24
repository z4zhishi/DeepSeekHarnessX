// Package spill: oversized tool-result spill storage (upstream
// CK/packages/spill: the Service Definition + the local filesystem provider +
// the spill-policy consumer, folded into one file-disjoint helper here).
//
// When a plain-text tool result grows past a byte threshold it would dominate
// the model context. This seam persists the FULL text to a private,
// session-scoped file (0600, owner-only, unpredictable name, traversal-safe)
// and returns a bounded head/tail preview plus the file locator and retrieval
// guidance — the model keeps the preview in context and can reach the full
// text through the locator without an unbounded inline blob.
//
// Save is best-effort from the caller's perspective: any storage failure or a
// sub-threshold result returns the original text unchanged so a spill can
// never hide a successful tool call or turn it into an error (upstream
// spill-policy treats a saveText rejection as keep-inline).
package tools

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// DefaultSpillMaxBytes is the maximum UTF-8 size of a plain-text tool result
// before it spills to a session-scoped file (upstream spill-policy
// maxInlineBytes default is 30–60KB; 40KB is the suggested mid-range).
const DefaultSpillMaxBytes = 40 * 1024

var (
	spillRootOnce sync.Once
	spillRootDir  string
	spillRootErr  error
)

// privateSpillRoot lazily creates one per-process private (0700) directory
// under the OS temp dir with an unpredictable suffix. Predictable world-readable
// paths would let other local users read spilled tool output or pre-plant a
// symlink (upstream spill-local privateRoot via mkdtemp).
func privateSpillRoot() (string, error) {
	spillRootOnce.Do(func() {
		spillRootDir, spillRootErr = os.MkdirTemp("", "dsh-spill-")
		if spillRootErr == nil {
			// MkdirTemp already creates 0700; keep it explicit and owner-only.
			spillRootErr = os.Chmod(spillRootDir, 0o700)
		}
	})
	return spillRootDir, spillRootErr
}

// sessionSpillDir derives a stable, collision-resistant directory for one
// session: `<root>/session-<sha256(sessionID)[:12]>` (upstream sessionDir).
func sessionSpillDir(root, sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(root, "session-"+hex.EncodeToString(sum[:6]))
}

// randomHex returns n random bytes as lowercase hex (unpredictable name).
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand never fails in practice; fall back to a time-based value
		// so a name is still produced (best-effort seam).
		return hex.EncodeToString([]byte(fmt.Sprintf("%d", os.Getpid())))
	}
	return hex.EncodeToString(b)
}

// encodeSegment maps an arbitrary string onto one safe path segment,
// injectively over all strings. Session ids / suggested names are untrusted
// input, so `../`, absolute paths, NUL and separators are neutralized before
// any filesystem use. Safe literal code points are kept; everything else is
// escaped as `~XXXX`. `.` / `..` are escaped so they can never traverse; an
// empty string encodes to `~`. (Mirrors the JSONL persistence backend's
// encodeSegment and spill-local's store.encodeSegment.)
func encodeSegment(raw string) string {
	if raw == "" {
		return "~"
	}
	if raw == "." {
		return "~002E"
	}
	if raw == ".." {
		return "~002E~002E"
	}
	var out strings.Builder
	for _, r := range raw {
		if r != '~' && (r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' ||
			r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			out.WriteRune(r)
		} else {
			fmt.Fprintf(&out, "~%04X", r)
		}
	}
	return out.String()
}

// trimTrailingPartialUtf8 drops a trailing incomplete UTF-8 sequence so a
// prefix cut never emits a replacement char at the boundary (upstream
// output-retention).
func trimTrailingPartialUtf8(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	i := len(b) - 1
	for i >= 0 && (b[i]&0xc0) == 0x80 && len(b)-i <= 3 {
		i--
	}
	if i < 0 {
		return b
	}
	lead := b[i]
	expected := 1
	switch {
	case lead < 0x80:
		expected = 1
	case lead < 0xe0:
		expected = 2
	case lead < 0xf0:
		expected = 3
	case lead < 0xf8:
		expected = 4
	default:
		expected = 0
	}
	if expected == 0 {
		return b
	}
	if len(b)-i < expected {
		return b[:i]
	}
	return b
}

// trimLeadingContinuationUtf8 drops leading continuation bytes so a suffix
// cut starts on a lead/ASCII byte instead of mid-codepoint (upstream
// output-retention).
func trimLeadingContinuationUtf8(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i]&0xc0) == 0x80 {
		i++
	}
	return b[i:]
}

// truncateBytesHead keeps at most n leading bytes of s without splitting a
// UTF-8 rune.
func truncateBytesHead(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return string(trimTrailingPartialUtf8([]byte(s[:n])))
}

// truncateBytesTail keeps at most n trailing bytes of s without splitting a
// UTF-8 rune.
func truncateBytesTail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return string(trimLeadingContinuationUtf8([]byte(s[len(s)-n:])))
}

// spillNotice builds the model-facing notice: how much was omitted plus the
// opaque locator and backend retrieval guidance (upstream spillNotice).
func spillNotice(omittedBytes int, locator string) string {
	return fmt.Sprintf(
		"(%d bytes omitted. Full formatted result stored at: %s. Use read with offset/limit, or grep this path to search within it.)",
		omittedBytes, locator)
}

// spillPreview builds the bounded head/tail replacement text for a spilled
// result. It mirrors the upstream spill-policy projection: a head+tail preview
// bounded by the same budget that triggered the spill, then the notice line.
func spillPreview(text, locator string, budget int) string {
	headBytes := budget / 2
	tailBytes := budget - headBytes
	head := truncateBytesHead(text, headBytes)
	tail := truncateBytesTail(text, tailBytes)
	kept := len(head) + len(tail)
	omitted := len(text) - kept
	if omitted < 0 {
		omitted = 0
	}
	return head + tail + "\n\n" + spillNotice(omitted, locator)
}

// Save persists an oversized plain-text tool result to the session's private
// spill directory and returns the bounded head/tail preview with locator and
// retrieval guidance. When `text` is at or below DefaultSpillMaxBytes it
// returns the text unchanged (no file is written). kind is the producing tool
// name used to derive a readable, sanitized suggested filename.
//
// Best-effort: any storage failure returns (text, err) — the caller keeps the
// original inline result (upstream spill-policy degradation).
func Save(sessionID, kind, text string) (string, error) {
	if len(text) <= DefaultSpillMaxBytes {
		return text, nil
	}
	root, err := privateSpillRoot()
	if err != nil {
		return text, err
	}
	dir := sessionSpillDir(root, sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return text, err
	}
	// Unpredictable name: random hex prefix + sanitized suggested name, so a
	// pre-planted symlink in the shared root cannot redirect the write and the
	// name stays readable for inspection.
	name := randomHex(6) + "-" + encodeSegment(kind+".txt")
	path := filepath.Join(dir, name)
	// Exclusive + owner-only ('O_EXCL', 0600): fails on any existing path —
	// symlink or not — so a planted target cannot hijack the write.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return text, err
	}
	if _, werr := f.WriteString(text); werr != nil {
		_ = f.Close()
		return text, werr
	}
	if cerr := f.Close(); cerr != nil {
		return text, cerr
	}
	return spillPreview(text, path, DefaultSpillMaxBytes), nil
}

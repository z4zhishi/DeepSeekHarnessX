// Package instructions resolves the community instruction-file family
// (AGENTS.md, CLAUDE.md + CLAUDE.local.md, .claude/rules, GEMINI.md, QWEN.md,
// .github/copilot-instructions, .cursor/rules, .clinerules, windsurf, devin)
// into the system prompt seam. One plain-Markdown grammar family; deterministic
// injection order (user-global files first, then ancestors root->cwd); scoped
// rules are always included and annotated with their glob so the model
// self-applies them (v1: include-with-glob-annotation, no runtime glob
// matching or file watcher); @path imports expand up to 4 hops, cycle-safe,
// relative to the including file.
package instructions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// MaxTotalBytes caps the resolved prompt injected into the system seam. The
// cap protects the context window from one gigantic memory tree; when hit,
// the excess bytes are dropped and a "truncation" detail source records it.
const MaxTotalBytes = 32 << 10 // 32KiB

// maxImportDepth bounds @import expansion: a chain longer than 4 hops stops
// expanding (the remaining @tokens are dropped, not followed).
const maxImportDepth = 4

// InstructionSource records one file (or event) that contributed to the
// resolved system prompt. Sources are returned in injection order so a host
// can surface provenance (e.g. a host.describe source count).
type InstructionSource struct {
	// Path is the absolute file path read ("" for the truncation entry).
	Path string
	// Rel is the display path relative to the resolved cwd ("~/..." for
	// user-global files, absolute when outside both).
	Rel string
	// Family is the tool family the file belongs to:
	// AGENTS | CLAUDE | GEMINI | QWEN | COPILOT | CURSOR | CLINE | WINDSURF
	// | DEVIN | import | truncation.
	Family string
	// Scope is the normalized glob scope carried by the file's frontmatter
	// (globs: / paths: / applyTo: / trigger:). Empty for unscoped files.
	Scope string
	// Imported marks files reached via @import expansion.
	Imported bool
	// Bytes is the contributing body size (frontmatter stripped, trimmed;
	// for the truncation entry: the capped total).
	Bytes int
	// Error is a non-fatal read error ("" when the file was read cleanly).
	Error string
}

// resolver carries the per-Resolve state: injection order, cycle/dedupe
// bookkeeping, the assembled prompt parts and the provenance detail.
type resolver struct {
	// anchor is the ancestor-walk root; display paths are project-relative
	// to it (root view), like the workspace-relative path set the globs
	// scope against.
	anchor  string
	cwd     string
	home    string
	seen    map[string]bool
	onStack map[string]bool
	sources []InstructionSource
	parts   []string
}

// walkRoot is the ancestor-walk boundary seam. Production resolves the git
// root (os.Stat of .git) or the filesystem root; tests pin it to a temp
// directory so resolution stays hermetic against real ancestor files.
var walkRoot = detectAncestorRoot

// userHomeDir is the UserHomeDir seam; the DSHX_TEST_HOME environment
// variable takes precedence (documented test hook for global-file injection).
var userHomeDir = os.UserHomeDir

func userHome() string {
	if h := strings.TrimSpace(os.Getenv("DSHX_TEST_HOME")); h != "" {
		return h
	}
	if h, err := userHomeDir(); err == nil {
		return h
	}
	return ""
}

// Resolve reads the community instruction-file family for cwd and returns the
// assembled system text plus the per-file detail. Deterministic: no
// filesystem timestamps or map iteration order influence the output.
//
// Order (2026-08 survey, CC ancestor-concat semantics):
//  1. user-global: ~/.agents/AGENTS.md, ~/.claude/CLAUDE.md, ~/.gemini/GEMINI.md
//  2. project ancestors from the walk root down to cwd, each directory's
//     family files in family order (AGENTS.md, then CLAUDE.md + CLAUDE.local.md
//     + .claude/CLAUDE.md + .claude/rules/*.md, GEMINI.md, QWEN.md, copilot,
//     cursor, cline, windsurf/devin), root first and cwd last.
//
// Missing files are skipped silently (the common case); unreadable files are
// recorded in the detail with Error set. An error return means resolution
// could not be attempted at all (invalid cwd); per-file problems never fail.
func Resolve(cwd string) (string, []InstructionSource, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", nil, fmt.Errorf("instructions: resolve cwd: %w", err)
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", nil, fmt.Errorf("instructions: cwd is not a directory: %s", abs)
	}
	r := &resolver{
		anchor:  filepath.Clean(walkRoot(abs)),
		cwd:     abs,
		home:    userHome(),
		seen:    map[string]bool{},
		onStack: map[string]bool{},
	}

	// 1) User-global files first (they apply to every workspace).
	if r.home != "" {
		r.addFile(filepath.Join(r.home, ".agents", "AGENTS.md"), "AGENTS", nil)
		r.addFile(filepath.Join(r.home, ".claude", "CLAUDE.md"), "CLAUDE", nil)
		r.addFile(filepath.Join(r.home, ".gemini", "GEMINI.md"), "GEMINI", nil)
	}

	// 2) Ancestor chain, root first, cwd last (CC ancestor concat).
	for _, dir := range ancestorChain(abs) {
		r.addDir(dir)
	}

	out := strings.Join(r.parts, "\n\n")
	if len(out) > MaxTotalBytes {
		out = truncAtRuneBoundary(out, MaxTotalBytes)
		r.sources = append(r.sources, InstructionSource{
			Family: "truncation",
			Bytes:  len(out),
			Error:  fmt.Sprintf("exceeds %d bytes; truncated at cap", MaxTotalBytes),
		})
	}
	return out, r.sources, nil
}

// detectAncestorRoot bounds the ancestor walk: the first directory at or
// above cwd whose .git exists (directory or worktree file) is the repo root;
// without a hit the filesystem root bounds the walk.
func detectAncestorRoot(cwd string) string {
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

// ancestorChain lists cwd's directories up to (and including) the walk root,
// highest ancestor first and cwd last.
func ancestorChain(cwd string) []string {
	root := filepath.Clean(walkRoot(cwd))
	var up []string
	for dir := cwd; ; {
		up = append(up, dir)
		if filepath.Clean(dir) == root {
			break
		}
		parent := filepath.Dir(dir)
		// Safety net: never loop past the filesystem root even if the
		// walkRoot seam returned a directory outside cwd's tree.
		if parent == dir {
			break
		}
		dir = parent
	}
	for i, j := 0, len(up)-1; i < j; i, j = i+1, j-1 {
		up[i], up[j] = up[j], up[i]
	}
	return up
}

// addDir reads the full family file set of one directory in canonical order.
func (r *resolver) addDir(dir string) {
	r.addFile(filepath.Join(dir, "AGENTS.md"), "AGENTS", nil)

	r.addFile(filepath.Join(dir, "CLAUDE.md"), "CLAUDE", nil)
	r.addFile(filepath.Join(dir, "CLAUDE.local.md"), "CLAUDE", nil)
	r.addFile(filepath.Join(dir, ".claude", "CLAUDE.md"), "CLAUDE", nil)
	r.addRulesDir(filepath.Join(dir, ".claude", "rules"), ".md", "CLAUDE", "paths")

	r.addFile(filepath.Join(dir, "GEMINI.md"), "GEMINI", nil)
	r.addFile(filepath.Join(dir, "QWEN.md"), "QWEN", nil)

	r.addFile(filepath.Join(dir, ".github", "copilot-instructions.md"), "COPILOT", nil)
	r.addRulesDir(filepath.Join(dir, ".github", "instructions"), ".instructions.md", "COPILOT", "applyTo")

	r.addRulesDir(filepath.Join(dir, ".cursor", "rules"), ".mdc", "CURSOR", "globs")
	r.addFile(filepath.Join(dir, ".cursorrules"), "CURSOR", nil)

	r.addRulesDir(filepath.Join(dir, ".clinerules"), ".md", "CLINE", "paths")

	r.addFile(filepath.Join(dir, ".windsurfrules"), "WINDSURF", nil)
	r.addRulesDir(filepath.Join(dir, ".windsurf", "rules"), ".md", "WINDSURF", "globs", "trigger")
	r.addRulesDir(filepath.Join(dir, ".devin", "rules"), ".md", "DEVIN", "globs", "trigger")
}

// addRulesDir reads every *suffix file of a rules directory (sorted for
// determinism) with the given frontmatter scope key(s).
func (r *resolver) addRulesDir(dir, suffix, family string, scopeKeys ...string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		names = append(names, e.Name())
	}
	// Deterministic regardless of directory read order.
	sort.Strings(names)
	for _, n := range names {
		r.addFile(filepath.Join(dir, n), family, scopeKeys)
	}
}

// addFile reads one instruction file and appends its body. Already-seen paths
// (home/ancestor overlap, @imports, repeated keys) are dropped
// deterministically. Missing files are skipped silently.
func (r *resolver) addFile(path, family string, scopeKeys []string) {
	if path == "" || r.seen[path] {
		return
	}
	r.seen[path] = true
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			r.sources = append(r.sources, InstructionSource{
				Path: path, Rel: r.displayPath(path), Family: family, Error: err.Error(),
			})
		}
		return
	}
	if text := r.render(path, family, data, false, scopeKeys); text != "" {
		r.parts = append(r.parts, text)
	}
}

// render splits frontmatter, expands @imports and returns the annotated body.
// An empty return means the file contributed nothing (empty body).
func (r *resolver) render(path, family string, data []byte, imported bool, scopeKeys []string) string {
	fields, body := splitFrontmatter(string(data))
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	scope := scopeText(fields, scopeKeys)
	r.onStack[path] = true
	body = r.expandImports(path, body, 0)
	delete(r.onStack, path)
	if ann := scopeAnnotation(scope); ann != "" {
		body = ann + "\n" + body
	}
	r.sources = append(r.sources, InstructionSource{
		Path: path, Rel: r.displayPath(path), Family: family,
		Scope: scope, Imported: imported, Bytes: len(body),
	})
	return body
}

// importToken matches a single @path token. The conservative character set
// keeps sentence punctuation out of candidate paths.
var importToken = regexp.MustCompile(`@([A-Za-z0-9_\-./~]+)`)

// expandImports replaces @path tokens outside code spans with the referenced
// file content, relative to the including file. Already-imported files,
// cycles and depth-overflow tokens are dropped; a token whose target does not
// exist (any @word in prose, including emails) is left untouched. Directory
// targets are not followed (v1).
func (r *resolver) expandImports(including, body string, depth int) string {
	if !strings.Contains(body, "@") {
		return body
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "@") {
			continue
		}
		if !strings.Contains(line, "`") {
			lines[i] = r.expandLine(including, line, depth)
			continue
		}
		// Backtick-odd segments are inside code spans: skip them so wrapped
		// @paths stay literal.
		segs := strings.Split(line, "`")
		for j := 0; j < len(segs); j += 2 {
			segs[j] = r.expandLine(including, segs[j], depth)
		}
		lines[i] = strings.Join(segs, "`")
	}
	return strings.Join(lines, "\n")
}

func (r *resolver) expandLine(including, line string, depth int) string {
	return importToken.ReplaceAllStringFunc(line, func(tok string) string {
		return r.expandOne(including, tok, depth)
	})
}

// expandOne expands one @token. Returning tok verbatim keeps user text that
// merely looks like a path on disk (emails, URLs without a real file).
func (r *resolver) expandOne(including, tok string, depth int) string {
	raw := tok[1:]
	if depth+1 > maxImportDepth {
		return ""
	}
	resolved, ok := r.resolveImportPath(including, raw)
	if !ok || r.seen[resolved] || r.onStack[resolved] {
		return ""
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return tok
	}
	r.seen[resolved] = true
	if text := r.renderImport(resolved, data, depth); text != "" {
		return text
	}
	return ""
}

// renderImport is render() for @import provenance: the family is fixed, the
// path is already registered as seen, and the on-stack window is managed
// here so a cycle back to the importing file is rejected.
func (r *resolver) renderImport(path string, data []byte, depth int) string {
	fields, body := splitFrontmatter(string(data))
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	scope := scopeText(fields, nil)
	r.onStack[path] = true
	body = strings.TrimSpace(r.expandImports(path, body, depth+1))
	delete(r.onStack, path)
	if body == "" {
		return ""
	}
	if ann := scopeAnnotation(scope); ann != "" {
		body = ann + "\n" + body
	}
	r.sources = append(r.sources, InstructionSource{
		Path: path, Rel: r.displayPath(path), Family: "import",
		Imported: true, Bytes: len(body),
	})
	return body
}

func (r *resolver) resolveImportPath(including, raw string) (string, bool) {
	switch {
	case raw == "":
		return "", false
	case strings.HasPrefix(raw, "~/"):
		if r.home == "" {
			return "", false
		}
		return filepath.Join(r.home, raw[2:]), true
	case filepath.IsAbs(raw):
		return filepath.Clean(raw), true
	default:
		return filepath.Join(filepath.Dir(including), raw), true
	}
}

// splitFrontmatter strips a leading "---" YAML block and parses it into a
// top-level field map. Text without frontmatter is returned unchanged; an
// unparsable frontmatter block is ignored (body starts after it).
func splitFrontmatter(text string) (map[string]any, string) {
	text = strings.TrimPrefix(text, "\ufeff") // Windows-authored BOM
	lines := strings.Split(text, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
		return nil, text
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			body := strings.Join(lines[i+1:], "\n")
			var fields map[string]any
			if err := yaml.Unmarshal([]byte(strings.Join(lines[1:i], "\n")), &fields); err != nil {
				return nil, body
			}
			return fields, body
		}
	}
	return nil, text
}

// scopeText renders the scope keys found in frontmatter into one annotation
// string. An explicit glob scope always wins; a trigger: frontmatter that is
// not always_on is itself worth annotating (windsurf/devin rules). trigger
// is deliberately excluded from the glob-key loop: its value is a mode, not
// a path pattern.
func scopeText(fields map[string]any, scopeKeys []string) string {
	if len(fields) == 0 {
		return ""
	}
	var out []string
	for _, k := range scopeKeys {
		if k == "trigger" {
			continue
		}
		if v, ok := fields[k]; ok {
			if s := globListString(v); s != "" {
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		if t, ok := fields["trigger"].(string); ok && t != "" && t != "always_on" {
			out = append(out, "trigger:"+t)
		}
	}
	return strings.Join(out, ", ")
}

// globListString normalizes a scalar or YAML list field to a comma-joined
// string; non-string list items are skipped.
func globListString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				parts = append(parts, strings.TrimSpace(s))
			}
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

// scopeAnnotation prefixes a scoped rule body. v1 includes scoped rules
// unconditionally; the annotation asks the model to self-apply the glob.
func scopeAnnotation(scope string) string {
	if scope == "" {
		return ""
	}
	return "(applies to workspace paths matching: " + scope + ")"
}

// displayPath renders provenance: relative to the ancestor-walk root when
// inside the project tree (root view, matching the workspace-relative path
// set the globs scope against), "~/..." when inside the home, otherwise the
// absolute path; backslashes are normalized so hosts on every platform see
// slash paths.
func (r *resolver) displayPath(path string) string {
	if rel, err := filepath.Rel(r.anchor, path); err == nil && !relEscape(rel) {
		return filepath.ToSlash(rel)
	}
	if r.home != "" {
		if rel, err := filepath.Rel(r.home, path); err == nil && !relEscape(rel) {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(path)
}

func relEscape(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, "..\\") || strings.HasPrefix(rel, "../")
}

// truncAtRuneBoundary cuts to at most max bytes without splitting a UTF-8
// sequence so the capped prompt stays decodable.
func truncAtRuneBoundary(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for i := 0; i < utf8.UTFMax && len(cut) > 0; i++ {
		if r, _ := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut
}

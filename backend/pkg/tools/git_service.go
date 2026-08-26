// GitService is the backend of the gateway git.* RPC family (git.status /
// git.diff / git.log / git.branches / git.stage / git.unstage / git.commit /
// git.discard / git.detect). It binds one workspace directory and exposes the
// IDE source-control data plane on top of the git CLI.
//
// Security posture:
//   - Every invocation is exec.CommandContext(ctx, "git", "-C", root, ...)
//     with a fixed argv array. No shell is involved anywhere, so shell
//     metacharacters cannot turn user input into commands.
//   - User-controlled strings may only land in pathspec position, after an
//     explicit "--" separator, so they can never be parsed as git options
//     (--upload-pack style injection is structurally impossible).
//   - Pathspecs are cleaned and validated to be relative and contained within
//     the workspace root; absolute paths and ".." escapes are rejected before
//     git ever sees them.
//   - The workspace root must itself be a repository root (.git present AND
//     `rev-parse --show-toplevel` equal to it), so status paths (repo-relative
//     by porcelain contract) always line up with validated pathspecs.
//   - Read-only commands run with GIT_OPTIONAL_LOCKS=0 (no opportunistic index
//     refresh writes) and GIT_TERMINAL_PROMPT=0 (a credential prompt cannot
//     hang past the per-command timeout).
//
// Write operations are limited to stage/unstage/commit/discard. Commit uses a
// single -m argument (never an editor) and still runs pre-commit hooks; a hook
// failure surfaces verbatim through the structured error detail. Discard
// requires confirm=true from the caller.
package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotGitRepo is the sentinel wrapped by every operation invoked outside a
// git repository (errors.Is-compatible), matching the "not a repository"
// structured-error envelope required by the RPC surface.
var ErrNotGitRepo = errors.New("not a git repository")

// Per-command timeouts. status/diff-class reads get 10s; commit/log-class and
// mutations get 15s (pre-commit hooks may legitimately take longer).
const (
	gitFastTimeout = 10 * time.Second
	gitSlowTimeout = 15 * time.Second
)

// GitDiffMaxBytes caps the returned patch text; larger diffs are truncated
// and flagged via GitDiffResult.Truncated.
const GitDiffMaxBytes = 512 << 10

// GitError is the structured failure type surfaced through the gateway error
// envelope. Code is a stable machine-readable tag (not_a_repo, timeout,
// confirm_required, ...) and Detail carries git's stderr verbatim so
// pre-commit-hook output reaches the UI unfiltered.
type GitError struct {
	Op     string // operation name, e.g. "status", "commit"
	Code   string // stable classification tag
	Detail string // human-readable explanation (usually raw git stderr)
	cause  error
}

func (e *GitError) Error() string {
	return fmt.Sprintf("git %s [%s]: %s", e.Op, e.Code, e.Detail)
}

func (e *GitError) Unwrap() error { return e.cause }

// GitService runs git against one bound workspace root. It is stateless and
// safe for concurrent use. Env, when non-empty, replaces os.Environ() for
// spawned git processes (tests isolate global config this way).
type GitService struct {
	root string
	// Env overrides the child-process environment when non-nil/non-empty.
	Env []string
}

// NewGitService binds a GitService to the given workspace root. The root is
// canonicalized best-effort (absolute path, symlinks and Windows 8.3 short
// names resolved) so it compares equal to what git itself reports.
func NewGitService(workspaceRoot string) *GitService {
	if abs, err := filepath.Abs(workspaceRoot); err == nil {
		workspaceRoot = abs
	}
	root := filepath.Clean(workspaceRoot)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		if abs, err := filepath.Abs(resolved); err == nil {
			root = filepath.Clean(abs)
		}
	}
	return &GitService{root: root}
}

// Root reports the bound workspace root.
func (g *GitService) Root() string { return g.root }

// env builds the child environment: caller override (tests) else the process
// environment, always plus the safety toggles described on the package doc.
func (g *GitService) env() []string {
	base := os.Environ()
	if len(g.Env) > 0 {
		base = g.Env
	}
	return append(base[:len(base):len(base)],
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
	)
}

// run executes one git command with `-C root` prepended and a hard timeout.
// argv elements are passed verbatim as arguments; callers must keep
// user-controlled strings behind "--".
func (g *GitService) run(ctx context.Context, timeout time.Duration, args ...string) (stdout, stderr string, err error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := make([]string, 0, len(args)+2)
	argv = append(argv, "-C", g.root)
	argv = append(argv, args...)
	cmd := exec.CommandContext(cctx, "git", argv...)
	cmd.Env = g.env()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	if err != nil && cctx.Err() != nil {
		// The failure coincides with deadline/cancel; normalize platform-
		// dependent kill semantics into the canonical deadline error.
		err = fmt.Errorf("%w (command exceeded %s timeout)", context.DeadlineExceeded, timeout)
	}
	return out.String(), errb.String(), err
}

// classifyGitErr turns a failed run into a structured *GitError, recognizing
// the handful of git signatures worth their own stable code.
func classifyGitErr(op string, err error, stderr string) error {
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		detail = err.Error()
	}
	low := strings.ToLower(detail)
	code := "git_failed"
	var cause error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		code, cause = "timeout", context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		code, cause = "canceled", context.Canceled
	case strings.Contains(low, "not a git repository"):
		code, cause = "not_a_repo", ErrNotGitRepo
	case strings.Contains(low, "nothing to commit"):
		code = "nothing_to_commit"
	case strings.Contains(low, "did not match any file"):
		code = "pathspec_no_match"
	case strings.Contains(low, "permission denied"), strings.Contains(low, "access is denied"):
		code = "permission_denied"
	}
	return &GitError{Op: op, Code: code, Detail: detail, cause: cause}
}

// sameDir reports whether two path spellings denote the same directory.
// The primary test is os.SameFile (identity via volume serial + file index),
// which correctly equates Windows 8.3 short names, symlinked and case-varied
// spellings; a normalized string comparison is the fallback when either side
// cannot be stat'ed.
func sameDir(a, b string) bool {
	norm := func(p string) string {
		p = strings.TrimSpace(p)
		return filepath.Clean(filepath.FromSlash(p))
	}
	na, nb := norm(a), norm(b)
	if na == "" || nb == "" {
		return false
	}
	if ia, err := os.Stat(na); err == nil {
		if ib, err2 := os.Stat(nb); err2 == nil {
			return os.SameFile(ia, ib)
		}
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(na, nb)
	}
	return na == nb
}

// ensureRepo verifies the bound root hosts a repository: .git must exist
// directly under it AND rev-parse --show-toplevel must resolve to the same
// directory. The equality check keeps porcelain status paths (always
// repo-root-relative) aligned with the pathspec validation domain.
func (g *GitService) ensureRepo(ctx context.Context) error {
	if g.root == "" || g.root == "." {
		return &GitError{Op: "detect", Code: "not_a_repo",
			Detail: "workspace root is not configured", cause: ErrNotGitRepo}
	}
	if _, err := os.Stat(filepath.Join(g.root, ".git")); err != nil {
		return &GitError{Op: "detect", Code: "not_a_repo",
			Detail: fmt.Sprintf("no .git under workspace root %s", g.root), cause: ErrNotGitRepo}
	}
	out, stderr, err := g.run(ctx, gitFastTimeout, "rev-parse", "--show-toplevel")
	if err != nil {
		return classifyGitErr("detect", err, stderr)
	}
	top := strings.TrimSpace(out)
	if top == "" || !sameDir(top, g.root) {
		return &GitError{Op: "detect", Code: "not_a_repo",
			Detail: fmt.Sprintf("repository root %q does not match workspace root %q", top, g.root),
			cause:  ErrNotGitRepo}
	}
	return nil
}

// validateRelPaths cleans and validates pathspec candidates: relative,
// inside the workspace root, no NUL bytes. Returns deduplicated forward-slash
// paths safe to place after "--".
func (g *GitService) validateRelPaths(op string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, &GitError{Op: op, Code: "invalid_path", Detail: "paths must not be empty"}
	}
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, raw := range paths {
		if raw == "" {
			return nil, &GitError{Op: op, Code: "invalid_path", Detail: "path entries must not be empty"}
		}
		if strings.ContainsRune(raw, 0) {
			return nil, &GitError{Op: op, Code: "invalid_path",
				Detail: fmt.Sprintf("path contains a NUL byte: %q", raw)}
		}
		p := filepath.Clean(filepath.FromSlash(raw))
		if filepath.IsAbs(p) || strings.HasPrefix(p, "\\") || (len(p) > 1 && p[1] == ':') {
			return nil, &GitError{Op: op, Code: "invalid_path",
				Detail: fmt.Sprintf("absolute paths are not allowed: %q", raw)}
		}
		p = filepath.ToSlash(p)
		if p == ".." || strings.HasPrefix(p, "../") {
			return nil, &GitError{Op: op, Code: "invalid_path",
				Detail: fmt.Sprintf("path escapes the workspace root: %q", raw)}
		}
		if _, dup := seen[p]; !dup {
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Detect
// ---------------------------------------------------------------------------

// GitDetectResult answers "is the workspace a git repository" plus enough
// context for the panel to render head info without a second round-trip.
type GitDetectResult struct {
	IsRepo   bool   `json:"isRepo"`
	Root     string `json:"root"`
	RepoRoot string `json:"repoRoot,omitempty"`
	Head     string `json:"head,omitempty"` // branch name when not detached
	Detached bool   `json:"detached,omitempty"`
	Sha      string `json:"sha,omitempty"`    // current HEAD sha (also when detached)
	Reason   string `json:"reason,omitempty"` // why IsRepo is false
}

// Detect dual-verifies repository membership: .git existence under the root
// AND `rev-parse --is-inside-work-tree`. It never returns an error for
// "simply not a repo" — that outcome is data (IsRepo=false + Reason).
func (g *GitService) Detect(ctx context.Context) (*GitDetectResult, error) {
	res := &GitDetectResult{Root: g.root}
	if g.root == "" || g.root == "." {
		res.Reason = "workspace root is not configured"
		return res, nil
	}
	if _, err := os.Stat(filepath.Join(g.root, ".git")); err != nil {
		res.Reason = fmt.Sprintf("no .git under %s", g.root)
		return res, nil
	}
	out, stderr, err := g.run(ctx, gitFastTimeout, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(out) != "true" {
		res.Reason = "git rev-parse --is-inside-work-tree did not report true"
		if detail := strings.TrimSpace(stderr); detail != "" {
			res.Reason += ": " + detail
		} else if err != nil {
			res.Reason += ": " + err.Error()
		}
		return res, nil
	}
	res.IsRepo = true
	if top, stderr, err := g.run(ctx, gitFastTimeout, "rev-parse", "--show-toplevel"); err == nil {
		res.RepoRoot = strings.TrimSpace(top)
	} else {
		_ = stderr
	}
	if br, _, err := g.run(ctx, gitFastTimeout, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		res.Head = strings.TrimSpace(br)
	} else {
		res.Detached = true
	}
	if sha, _, err := g.run(ctx, gitFastTimeout, "rev-parse", "HEAD"); err == nil {
		res.Sha = strings.TrimSpace(sha)
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// GitStatusEntry is one changed path. Index/Worktree carry porcelain v2 X/Y
// codes ("." means unchanged; "?" for untracked). OrigPath is set for
// rename/copy entries (worktree-side new path in Path).
type GitStatusEntry struct {
	Path       string `json:"path"`
	OrigPath   string `json:"origPath,omitempty"`
	Index      string `json:"index"`
	Worktree   string `json:"worktree"`
	Untracked  bool   `json:"untracked,omitempty"`
	Conflicted bool   `json:"conflicted,omitempty"`
}

// GitStatus is the parsed `status --porcelain=v2 --branch` document.
type GitStatus struct {
	Branch   string           `json:"branch"`             // "" or "(detached)" handled via Detached
	OID      string           `json:"oid,omitempty"`      // HEAD sha, "(initial)" pre-first-commit
	Upstream string           `json:"upstream,omitempty"` // e.g. origin/main; "" when none
	Ahead    int              `json:"ahead"`
	Behind   int              `json:"behind"`
	Detached bool             `json:"detached,omitempty"`
	Clean    bool             `json:"clean"`
	Entries  []GitStatusEntry `json:"entries"`
}

// Status parses `git status --porcelain=v2 --branch` into the structured
// shape consumed by the frontend source-control panel.
func (g *GitService) Status(ctx context.Context) (*GitStatus, error) {
	if err := g.ensureRepo(ctx); err != nil {
		return nil, err
	}
	out, stderr, err := g.run(ctx, gitFastTimeout, "status", "--porcelain=v2", "--branch")
	if err != nil {
		return nil, classifyGitErr("status", err, stderr)
	}
	return parsePorcelainV2(out), nil
}

// parsePorcelainV2 decodes porcelain v2 output (branch headers + ordinary/
// renamed/unmerged/untracked entries). Exported logic kept pure for tests.
func parsePorcelainV2(out string) *GitStatus {
	st := &GitStatus{Entries: []GitStatusEntry{}}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			name := strings.TrimPrefix(line, "# branch.head ")
			if name == "(detached)" {
				st.Detached = true
			} else {
				st.Branch = name
			}
		case strings.HasPrefix(line, "# branch.oid "):
			st.OID = strings.TrimPrefix(line, "# branch.oid ")
		case strings.HasPrefix(line, "# branch.upstream "):
			st.Upstream = strings.TrimPrefix(line, "# branch.upstream ")
		case strings.HasPrefix(line, "# branch.ab "):
			for _, f := range strings.Fields(strings.TrimPrefix(line, "# branch.ab ")) {
				if n, err := strconv.Atoi(strings.TrimLeft(f, "+-")); err == nil {
					if strings.HasPrefix(f, "+") {
						st.Ahead = n
					} else if strings.HasPrefix(f, "-") {
						st.Behind = n
					}
				}
			}
		case strings.HasPrefix(line, "1 "):
			// 1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>
			parts := strings.SplitN(line, " ", 9)
			if len(parts) < 9 {
				continue
			}
			x, y := xyCodes(parts[1])
			st.Entries = append(st.Entries, GitStatusEntry{
				Path: parts[8], Index: x, Worktree: y,
			})
		case strings.HasPrefix(line, "2 "):
			// 2 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <Xscore> <path>\t<origPath>
			parts := strings.SplitN(line, " ", 10)
			if len(parts) < 10 {
				continue
			}
			x, y := xyCodes(parts[1])
			entry := GitStatusEntry{Path: parts[9], Index: x, Worktree: y}
			if i := strings.IndexByte(entry.Path, '\t'); i >= 0 {
				entry.OrigPath = entry.Path[i+1:]
				entry.Path = entry.Path[:i]
			}
			st.Entries = append(st.Entries, entry)
		case strings.HasPrefix(line, "u "):
			// u <XY> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>
			parts := strings.SplitN(line, " ", 11)
			if len(parts) < 11 {
				continue
			}
			x, y := xyCodes(parts[1])
			st.Entries = append(st.Entries, GitStatusEntry{
				Path: parts[10], Index: x, Worktree: y, Conflicted: true,
			})
		case strings.HasPrefix(line, "? "):
			st.Entries = append(st.Entries, GitStatusEntry{
				Path: line[2:], Index: "?", Worktree: "?", Untracked: true,
			})
		}
	}
	st.Clean = len(st.Entries) == 0
	return st
}

// xyCodes splits an XY pair into its two codes, defaulting missing halves.
func xyCodes(xy string) (x, y string) {
	r := []rune(xy)
	x, y = ".", "."
	if len(r) > 0 {
		x = string(r[0])
	}
	if len(r) > 1 {
		y = string(r[1])
	}
	return x, y
}

// ---------------------------------------------------------------------------
// Diff
// ---------------------------------------------------------------------------

// GitFileStat is the numstat row for one changed file.
type GitFileStat struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary,omitempty"`
}

// GitDiffResult pairs the patch text with per-file statistics.
type GitDiffResult struct {
	Patch          string        `json:"patch"`
	Truncated      bool          `json:"truncated,omitempty"`
	Stats          []GitFileStat `json:"stats"`
	TotalAdditions int           `json:"totalAdditions"`
	TotalDeletions int           `json:"totalDeletions"`
}

// Diff returns the patch text plus per-file numstat for the worktree (default)
// or the staged area (staged=true), optionally scoped to one path. The patch
// and the statistics run concurrently; patch text beyond GitDiffMaxBytes is
// truncated at a line boundary and flagged.
func (g *GitService) Diff(ctx context.Context, path string, staged bool) (*GitDiffResult, error) {
	if err := g.ensureRepo(ctx); err != nil {
		return nil, err
	}
	var ps []string
	if path != "" {
		validated, err := g.validateRelPaths("diff", []string{path})
		if err != nil {
			return nil, err
		}
		ps = validated
	}
	patchArgs := []string{"diff"}
	numstatArgs := []string{"diff"}
	if staged {
		patchArgs = append(patchArgs, "--staged")
		numstatArgs = append(numstatArgs, "--staged")
	}
	numstatArgs = append(numstatArgs, "--numstat")
	if len(ps) > 0 {
		patchArgs = append(append(patchArgs, "--"), ps...)
		numstatArgs = append(append(numstatArgs, "--"), ps...)
	}

	type runRes struct {
		out, errs string
		err       error
	}
	patchCh := make(chan runRes, 1)
	numCh := make(chan runRes, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		o, e, er := g.run(ctx, gitFastTimeout, patchArgs...)
		patchCh <- runRes{o, e, er}
	}()
	go func() {
		defer wg.Done()
		o, e, er := g.run(ctx, gitFastTimeout, numstatArgs...)
		numCh <- runRes{o, e, er}
	}()
	wg.Wait()
	patchRun, numRun := <-patchCh, <-numCh
	if err := classifyGitErr("diff", patchRun.err, patchRun.errs); err != nil {
		return nil, err
	}
	if err := classifyGitErr("diff", numRun.err, numRun.errs); err != nil {
		return nil, err
	}

	res := &GitDiffResult{Patch: patchRun.out, Stats: []GitFileStat{}}
	for _, line := range strings.Split(numRun.out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		fstat := GitFileStat{Path: parts[2]}
		if parts[0] == "-" || parts[1] == "-" {
			fstat.Binary = true
		} else {
			fstat.Additions, _ = strconv.Atoi(parts[0])
			fstat.Deletions, _ = strconv.Atoi(parts[1])
			res.TotalAdditions += fstat.Additions
			res.TotalDeletions += fstat.Deletions
		}
		res.Stats = append(res.Stats, fstat)
	}
	if len(res.Patch) > GitDiffMaxBytes {
		cut := res.Patch[:GitDiffMaxBytes]
		// Back off to the last complete line near the cap so the frontend
		// never renders a torn hunk.
		if i := strings.LastIndexByte(cut, '\n'); i > GitDiffMaxBytes-8192 {
			cut = cut[:i+1]
		}
		res.Patch, res.Truncated = cut, true
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Log
// ---------------------------------------------------------------------------

// GitCommitInfo is one log entry. Timestamp is Unix seconds (%at).
type GitCommitInfo struct {
	Hash      string `json:"hash"`
	Abbrev    string `json:"abbrev"`
	Author    string `json:"author"`
	Timestamp int64  `json:"timestamp"`
	Subject   string `json:"subject"`
}

// GitLogResult is the paginated log page.
type GitLogResult struct {
	Commits []GitCommitInfo `json:"commits"`
}

// Log returns commits newest-first with pagination. limit defaults to 50 and
// clamps at 500; offset skips the newest N entries. An empty repository yields
// an empty list rather than an error.
func (g *GitService) Log(ctx context.Context, limit, offset int) (*GitLogResult, error) {
	if err := g.ensureRepo(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	// %x1f separates fields, %x1e terminates records: subjects containing "|"
	// or newlines cannot corrupt the framing.
	args := []string{
		"log",
		"--pretty=format:%H%x1f%h%x1f%an%x1f%at%x1f%s%x1e",
		"--max-count=" + strconv.Itoa(limit),
	}
	if offset > 0 {
		args = append(args, "--skip="+strconv.Itoa(offset))
	}
	out, stderr, err := g.run(ctx, gitSlowTimeout, args...)
	if err != nil {
		if strings.Contains(strings.ToLower(stderr), "does not have any commits yet") {
			return &GitLogResult{Commits: []GitCommitInfo{}}, nil
		}
		return nil, classifyGitErr("log", err, stderr)
	}
	return &GitLogResult{Commits: parseGitLog(out)}, nil
}

// parseGitLog decodes the %x1e/%x1f framed log format.
func parseGitLog(out string) []GitCommitInfo {
	commits := []GitCommitInfo{}
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n\r")
		rec = strings.TrimRight(rec, "\n\r")
		if rec == "" {
			continue
		}
		fields := strings.SplitN(rec, "\x1f", 5)
		if len(fields) < 5 {
			continue
		}
		ts, _ := strconv.ParseInt(strings.TrimSpace(fields[3]), 10, 64)
		commits = append(commits, GitCommitInfo{
			Hash:      fields[0],
			Abbrev:    fields[1],
			Author:    fields[2],
			Timestamp: ts,
			Subject:   fields[4],
		})
	}
	return commits
}

// ---------------------------------------------------------------------------
// Branches
// ---------------------------------------------------------------------------

// GitBranch is one local or remote-tracking ref.
type GitBranch struct {
	Name     string `json:"name"` // short name, e.g. main / origin/main
	Kind     string `json:"kind"` // "local" | "remote"
	FullRef  string `json:"fullRef"`
	Sha      string `json:"sha"`
	IsHead   bool   `json:"isHead,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Ahead    int    `json:"ahead,omitempty"`
	Behind   int    `json:"behind,omitempty"`
	Gone     bool   `json:"gone,omitempty"` // upstream configured but vanished
}

// GitBranchesResult lists refs plus the current checkout state.
type GitBranchesResult struct {
	Current  string      `json:"current,omitempty"`
	Detached bool        `json:"detached,omitempty"`
	Sha      string      `json:"sha,omitempty"`
	Branches []GitBranch `json:"branches"`
}

const gitBranchFormat = "%(refname)%09%(objectname)%09%(HEAD)%09%(upstream:short)%09%(upstream:track)"

// Branches enumerates local and remote-tracking branches with upstream
// tracking info, plus the currently checked-out ref (or detached HEAD).
func (g *GitService) Branches(ctx context.Context) (*GitBranchesResult, error) {
	if err := g.ensureRepo(ctx); err != nil {
		return nil, err
	}
	out, stderr, err := g.run(ctx, gitFastTimeout, "for-each-ref", "--format="+gitBranchFormat, "refs/heads", "refs/remotes")
	if err != nil {
		return nil, classifyGitErr("branches", err, stderr)
	}
	res := &GitBranchesResult{Branches: parseBranchRefs(out)}
	if cur, _, err := g.run(ctx, gitFastTimeout, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		res.Current = strings.TrimSpace(cur)
	} else if sha, _, shaErr := g.run(ctx, gitFastTimeout, "rev-parse", "HEAD"); shaErr == nil && strings.TrimSpace(sha) != "" {
		res.Detached = true
	}
	if sha, _, err := g.run(ctx, gitFastTimeout, "rev-parse", "HEAD"); err == nil {
		res.Sha = strings.TrimSpace(sha)
	}
	return res, nil
}

// parseBranchRefs decodes for-each-ref rows in gitBranchFormat layout.
func parseBranchRefs(out string) []GitBranch {
	branches := []GitBranch{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 5 {
			continue
		}
		refname, sha, headFlag, upstream, track :=
			parts[0], parts[1], parts[2], parts[3], parts[4]
		b := GitBranch{FullRef: refname, Sha: sha, Upstream: upstream}
		switch {
		case strings.HasPrefix(refname, "refs/heads/"):
			b.Kind = "local"
			b.Name = strings.TrimPrefix(refname, "refs/heads/")
		case strings.HasPrefix(refname, "refs/remotes/"):
			// Skip the remote HEAD pointer alias (origin/HEAD -> origin/main).
			if strings.HasSuffix(refname, "/HEAD") {
				continue
			}
			b.Kind = "remote"
			b.Name = strings.TrimPrefix(refname, "refs/remotes/")
		default:
			continue
		}
		b.IsHead = headFlag == "*"
		applyTrack(&b, track)
		branches = append(branches, b)
	}
	return branches
}

// applyTrack parses %(upstream:track) forms like "[ahead 2]",
// "[behind 1]", "[ahead 2, behind 1]", "[gone]".
func applyTrack(b *GitBranch, track string) {
	track = strings.TrimSpace(track)
	if track == "" {
		return
	}
	if strings.Contains(track, "gone") {
		b.Gone = true
		return
	}
	b.Ahead = trackCount(track, "ahead")
	b.Behind = trackCount(track, "behind")
}

// trackCount extracts the integer following key ("ahead"/"behind") inside a
// tracking string like "[ahead 2, behind 1]".
func trackCount(track, key string) int {
	i := strings.Index(track, key)
	if i < 0 {
		return 0
	}
	rest := strings.TrimLeft(track[i+len(key):], " ")
	if end := strings.IndexAny(rest, ",]"); end >= 0 {
		rest = rest[:end]
	}
	n, _ := strconv.Atoi(strings.TrimSpace(rest))
	return n
}

// ---------------------------------------------------------------------------
// Mutations: stage / unstage / commit / discard
// ---------------------------------------------------------------------------

// GitChangeResult reports how many distinct pathspecs were acted upon.
type GitChangeResult struct {
	Count int `json:"count"`
}

// Stage records the given paths (including untracked files) into the index:
// `git add -- <paths...>`.
func (g *GitService) Stage(ctx context.Context, paths []string) (*GitChangeResult, error) {
	if err := g.ensureRepo(ctx); err != nil {
		return nil, err
	}
	ps, err := g.validateRelPaths("stage", paths)
	if err != nil {
		return nil, err
	}
	_, stderr, err := g.run(ctx, gitSlowTimeout, append([]string{"add", "--"}, ps...)...)
	if err != nil {
		return nil, classifyGitErr("stage", err, stderr)
	}
	return &GitChangeResult{Count: len(ps)}, nil
}

// Unstage removes the given paths from the index, keeping worktree content:
// `git reset --quiet HEAD -- <paths...>`.
func (g *GitService) Unstage(ctx context.Context, paths []string) (*GitChangeResult, error) {
	if err := g.ensureRepo(ctx); err != nil {
		return nil, err
	}
	ps, err := g.validateRelPaths("unstage", paths)
	if err != nil {
		return nil, err
	}
	_, stderr, err := g.run(ctx, gitSlowTimeout, append([]string{"reset", "--quiet", "HEAD", "--"}, ps...)...)
	if err != nil {
		return nil, classifyGitErr("unstage", err, stderr)
	}
	return &GitChangeResult{Count: len(ps)}, nil
}

// GitCommitResult confirms a successful commit and reports the new HEAD sha.
type GitCommitResult struct {
	Committed bool   `json:"committed"`
	Sha       string `json:"sha"`
}

// Commit records the staged index with a single -m message (never an editor,
// never interactive). Pre-commit hooks run normally; a hook failure rejects
// the commit and its stderr is passed through in the error detail. Empty or
// whitespace-only messages are refused client-side.
func (g *GitService) Commit(ctx context.Context, message string) (*GitCommitResult, error) {
	if err := g.ensureRepo(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(message) == "" {
		return nil, &GitError{Op: "commit", Code: "empty_message",
			Detail: "commit message must not be empty"}
	}
	_, stderr, err := g.run(ctx, gitSlowTimeout, "commit", "-m", message)
	if err != nil {
		return nil, classifyGitErr("commit", err, stderr)
	}
	sha, stderr, err := g.run(ctx, gitFastTimeout, "rev-parse", "HEAD")
	if err != nil {
		return nil, classifyGitErr("commit", err, stderr)
	}
	return &GitCommitResult{Committed: true, Sha: strings.TrimSpace(sha)}, nil
}

// Discard restores a path from the index (`git checkout -- <path>`),
// permanently destroying uncommitted worktree changes for that path. It is
// refused unless the caller passes confirm=true.
func (g *GitService) Discard(ctx context.Context, path string, confirm bool) (*GitChangeResult, error) {
	if err := g.ensureRepo(ctx); err != nil {
		return nil, err
	}
	if !confirm {
		return nil, &GitError{Op: "discard", Code: "confirm_required",
			Detail: "discard refused: this runs `git checkout -- <path>` and permanently destroys " +
				"all uncommitted worktree changes for the path (content reverts to the last staged/" +
				"committed version); the operation cannot be undone. Retry with confirm=true."}
	}
	ps, err := g.validateRelPaths("discard", []string{path})
	if err != nil {
		return nil, err
	}
	_, stderr, err := g.run(ctx, gitSlowTimeout, "checkout", "--", ps[0])
	if err != nil {
		return nil, classifyGitErr("discard", err, stderr)
	}
	return &GitChangeResult{Count: 1}, nil
}

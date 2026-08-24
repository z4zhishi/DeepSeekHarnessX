// Package tools: filesystem manipulation tools.
//
// Stale-version edit protection (replace_file_content):
//
// A file may be edited by more than one agent/teammate concurrently. Without a
// version guard, two agents that both read a file, then both write their own
// edit, will clobber each other — the second writer silently overwrites the
// first writer's change because each operates on the snapshot it read. This is
// classic last-writer-wins data loss.
//
// Upstream (CK/packages/fs/tool-str-replace-editor + fs-local) solves this by
// anchoring every edit to the file version the caller observed. The edit carries
// an expected version; the writer re-stats the file and, if the on-disk version
// no longer matches, returns FS_STALE_VERSION instead of overwriting. This makes
// a concurrent edit observable: the second writer is told the file changed since
// it was read, and must re-read before retrying, so no edit is silently lost.
//
// We mirror that semantics here with a minimal, portable version anchor. Upstream
// uses `dev:ino:size:mtimeNs:ctimeNs` (CK fs-local/src/fsio.ts versionOf). Go's
// os.Stat does not expose inode/device/ctime portably (Windows in particular),
// so we anchor on the portable subset — file size + mtime (nanosecond) — which
// retains the freshness property that matters for the concurrent-edit race while
// staying platform-independent. Expected_version is OPTIONAL: callers that omit
// it keep the previous unconditional-edit behavior (backward compatible).
package tools

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// fileVersion computes the version anchor for stale-edit detection: file size
// plus modification time (nanosecond). Mirrors upstream's size:mtimeNs pairing;
// see the package comment for why dev/ino/ctime are omitted.
func fileVersion(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano()), nil
}

// RegisterFSTools registers full-scale file manipulation and code search tools.
func (r *ToolRegistry) RegisterFSTools() {
	// 1. read_file
	r.Register(ToolDefinition{
		Name:        "read_file",
		Description: "Read file contents with optional line range support.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": { "type": "string", "description": "Target file path" },
				"start_line": { "type": "integer", "description": "Optional starting line (1-indexed)" },
				"end_line": { "type": "integer", "description": "Optional ending line (1-indexed)" }
			},
			"required": ["path"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Path      string `json:"path"`
				StartLine int    `json:"start_line"`
				EndLine   int    `json:"end_line"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}

			targetPath := resolvePath(ctx.Cwd, args.Path)
			if r.Policy != nil {
				if marker := r.Policy.checkWrite(ctx.SessionID, ctx.Cwd, targetPath); marker != "" {
					return marker, nil
				}
			}
			data, err := os.ReadFile(targetPath)
			if err != nil {
				return nil, err
			}

			lines := strings.Split(string(data), "\n")
			if args.StartLine > 0 || args.EndLine > 0 {
				start := 1
				if args.StartLine > 1 {
					start = args.StartLine
				}
				end := len(lines)
				if args.EndLine > 0 && args.EndLine < len(lines) {
					end = args.EndLine
				}
				if start > len(lines) {
					return "", nil
				}
				if start > end {
					start = end
				}
				lines = lines[start-1 : end]
			}

			return strings.Join(lines, "\n"), nil
		},
	})

	// 2. write_file
	r.Register(ToolDefinition{
		Name:         "write_file",
		Description:  "Create or overwrite a file with contents.",
		RequiresPerm: true,
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": { "type": "string", "description": "Target file path" },
				"content": { "type": "string", "description": "Full file content" }
			},
			"required": ["path", "content"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}

			targetPath := resolvePath(ctx.Cwd, args.Path)
			// Sandbox boundary: the session's mode decides whether this write is
			// allowed (read-only denies everything; workspace-write confines to
			// the session workspace; danger-full-access passes). The denial is a
			// policy marker in the result, not a tool error — the model reads the
			// marker and escalates via the user-approval flow (upstream sandbox
			// denial vocabulary).
			if r.Policy != nil {
				if marker := r.Policy.checkWrite(ctx.SessionID, ctx.Cwd, targetPath); marker != "" {
					return marker, nil
				}
			}
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(targetPath, []byte(args.Content), 0644); err != nil {
				return nil, err
			}
			return fmt.Sprintf("Successfully wrote %d bytes to %s", len(args.Content), args.Path), nil
		},
	})

	// 3. replace_file_content
	//
	// Stale-version edit protection: the optional expected_version parameter lets
	// the caller pin the edit to the file version it read. When provided and the
	// on-disk version no longer matches (another agent edited the file in between),
	// the tool returns an FS_STALE_VERSION error instead of overwriting — the
	// caller must re-read and retry. When omitted, behavior is unchanged
	// (unconditional edit). See the package comment for the rationale.
	r.Register(ToolDefinition{
		Name:         "replace_file_content",
		Description:  "Replace target content with replacement content in a file.",
		RequiresPerm: true,
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": { "type": "string", "description": "Target file path" },
				"target_content": { "type": "string", "description": "Exact text to replace" },
				"replacement_content": { "type": "string", "description": "New replacement text" },
				"expected_version": { "type": "string", "description": "Optional file version the edit is based on. If the file changed since it was read (version mismatch), the edit is refused with FS_STALE_VERSION instead of overwriting." }
			},
			"required": ["path", "target_content", "replacement_content"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Path               string `json:"path"`
				TargetContent      string `json:"target_content"`
				ReplacementContent string `json:"replacement_content"`
				ExpectedVersion    string `json:"expected_version"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}

			targetPath := resolvePath(ctx.Cwd, args.Path)
			// Stale guard: verify the file still matches the version the caller
			// observed before reading/matching. Refuse with FS_STALE_VERSION rather
			// than silently overwrite a concurrent edit.
			if args.ExpectedVersion != "" {
				cur, err := fileVersion(targetPath)
				if err != nil {
					return nil, err
				}
				if cur != args.ExpectedVersion {
					return nil, fmt.Errorf(
						"cannot replace content in %s: file changed since it was read (FS_STALE_VERSION)", args.Path)
				}
			}

			data, err := os.ReadFile(targetPath)
			if err != nil {
				return nil, err
			}

			content := string(data)
			if !strings.Contains(content, args.TargetContent) {
				return nil, fmt.Errorf("target content not found in %s", args.Path)
			}

			newContent := strings.Replace(content, args.TargetContent, args.ReplacementContent, 1)
			if err := os.WriteFile(targetPath, []byte(newContent), 0644); err != nil {
				return nil, err
			}
			return fmt.Sprintf("Successfully replaced content in %s", args.Path), nil
		},
	})

	// 4. list_dir
	r.Register(ToolDefinition{
		Name:        "list_dir",
		Description: "List files and subdirectories in a directory.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": { "type": "string", "description": "Directory path" }
			},
			"required": ["path"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}

			targetPath := resolvePath(ctx.Cwd, args.Path)
			entries, err := os.ReadDir(targetPath)
			if err != nil {
				return nil, err
			}

			type Entry struct {
				Name  string `json:"name"`
				IsDir bool   `json:"is_dir"`
				Size  int64  `json:"size,omitempty"`
			}

			var result []Entry
			for _, e := range entries {
				info, _ := e.Info()
				var sz int64
				if info != nil && !e.IsDir() {
					sz = info.Size()
				}
				result = append(result, Entry{
					Name:  e.Name(),
					IsDir: e.IsDir(),
					Size:  sz,
				})
			}
			return result, nil
		},
	})

	// 5. find_by_name
	r.Register(ToolDefinition{
		Name:        "find_by_name",
		Description: "Search for files and directories matching a glob pattern.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": { "type": "string", "description": "Search root directory" },
				"pattern": { "type": "string", "description": "Glob pattern (e.g. *.go, *test*)" }
			},
			"required": ["path", "pattern"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Path    string `json:"path"`
				Pattern string `json:"pattern"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}

			targetPath := resolvePath(ctx.Cwd, args.Path)
			var matches []string

			_ = filepath.WalkDir(targetPath, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				// Skip git and node_modules
				if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == ".tools") {
					return filepath.SkipDir
				}

				matched, _ := filepath.Match(args.Pattern, d.Name())
				if matched {
					rel, _ := filepath.Rel(targetPath, p)
					matches = append(matches, rel)
					if len(matches) >= 100 {
						return fmt.Errorf("limit reached")
					}
				}
				return nil
			})

			return matches, nil
		},
	})

	// 6. grep_search
	r.Register(ToolDefinition{
		Name:        "grep_search",
		Description: "Search for regular expression or text occurrences within files.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": { "type": "string", "description": "Directory to search in" },
				"query": { "type": "string", "description": "Regex or string search query" },
				"case_insensitive": { "type": "boolean", "description": "Case insensitive search" }
			},
			"required": ["path", "query"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Path            string `json:"path"`
				Query           string `json:"query"`
				CaseInsensitive bool   `json:"case_insensitive"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}

			pattern := args.Query
			if args.CaseInsensitive {
				pattern = "(?i)" + pattern
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("invalid regex: %w", err)
			}

			targetPath := resolvePath(ctx.Cwd, args.Path)

			type Match struct {
				File       string `json:"file"`
				LineNumber int    `json:"line_number"`
				Content    string `json:"content"`
			}

			var matches []Match
			_ = filepath.WalkDir(targetPath, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					if d != nil && d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == ".tools") {
						return filepath.SkipDir
					}
					return nil
				}

				// Skip binary or large files
				if info, _ := d.Info(); info != nil && info.Size() > 5*1024*1024 {
					return nil
				}

				data, err := os.ReadFile(p)
				if err != nil {
					return nil
				}

				lines := strings.Split(string(data), "\n")
				rel, _ := filepath.Rel(targetPath, p)
				for i, line := range lines {
					if re.MatchString(line) {
						matches = append(matches, Match{
							File:       rel,
							LineNumber: i + 1,
							Content:    strings.TrimSpace(line),
						})
						if len(matches) >= 100 {
							return fmt.Errorf("limit reached")
						}
					}
				}
				return nil
			})

			return matches, nil
		},
	})
}

func resolvePath(cwd, p string) string {
	if filepath.IsAbs(p) || cwd == "" {
		return p
	}
	return filepath.Join(cwd, p)
}

package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// globMaxResults caps the paths retained inline by one glob call (upstream
// GLOB_MAX_RESULTS default of 100).
const globMaxResults = 100

// globVCSExcludes are directory names discovery never descends into (upstream
// GLOB_VCS_EXCLUDES): VCS metadata stores.
var globVCSExcludes = []string{".git", ".svn", ".hg", ".bzr", ".jj", ".sl"}

// globEntry is one discovered path with its modification time for sorting.
type globEntry struct {
	path string
	mod  time.Time
}

// globWalk discovers files under root whose path matches pattern, skipping
// VCS metadata directories. Patterns use filepath.Match semantics; a pattern
// with no wildcard is matched against each file name for direct-path
// requests.
func globWalk(root, pattern string) ([]globEntry, error) {
	var out []globEntry
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable subtrees, mirroring rg resilience
		}
		if d.IsDir() {
			for _, vcs := range globVCSExcludes {
				if d.Name() == vcs {
					return filepath.SkipDir
				}
			}
			return nil
		}
		matched := false
		if strings.ContainsAny(pattern, "*?[") {
			matched, _ = filepath.Match(pattern, d.Name())
			if !matched {
				// Also allow the pattern to match the full relative path so
				// patterns like **/*.go behave naturally under WalkDir.
				rel, relErr := filepath.Rel(root, path)
				if relErr == nil {
					matched, _ = filepath.Match(pattern, rel)
					if !matched {
						matched, _ = filepath.Match(filepath.ToSlash(pattern), filepath.ToSlash(rel))
					}
				}
			}
		} else {
			matched = d.Name() == pattern
		}
		if matched {
			if info, infoErr := d.Info(); infoErr == nil {
				out = append(out, globEntry{path: path, mod: info.ModTime()})
			} else {
				out = append(out, globEntry{path: path})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Modification-time order, newest first (upstream --sort=modified).
	sort.Slice(out, func(i, j int) bool { return out[i].mod.After(out[j].mod) })
	if len(out) > globMaxResults {
		out = out[:globMaxResults]
	}
	return out, nil
}

// RegisterGlobTool registers the glob tool (upstream @deepseek-ai/dsh-tool-
// fs-search/glob contract): discover files whose paths match a glob pattern,
// sorted by modification time, capped at globMaxResults.
func (r *ToolRegistry) RegisterGlobTool() {
	r.Register(ToolDefinition{
		Name:        "glob",
		Description: "Discover files whose paths match a glob pattern, sorted by modification time (newest first). Results are capped at 100 paths.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"pattern": { "type": "string", "description": "Glob pattern such as **/*.go or src/*.ts" },
				"path": { "type": "string", "description": "Search root; defaults to the session workspace" }
			},
			"required": ["pattern"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			if strings.TrimSpace(args.Pattern) == "" {
				return nil, fmt.Errorf("pattern must be a non-empty string")
			}
			root := ctx.Cwd
			if args.Path != "" {
				if filepath.IsAbs(args.Path) {
					root = args.Path
				} else if root != "" {
					root = filepath.Join(root, args.Path)
				}
			}
			entries, err := globWalk(root, args.Pattern)
			if err != nil {
				return nil, err
			}
			if len(entries) == 0 {
				return "No matching files.", nil
			}
			lines := make([]string, 0, len(entries))
			for _, e := range entries {
				rel, relErr := filepath.Rel(root, e.path)
				if relErr != nil {
					rel = e.path
				}
				lines = append(lines, filepath.ToSlash(rel))
			}
			return strings.Join(lines, "\n"), nil
		},
	})
}

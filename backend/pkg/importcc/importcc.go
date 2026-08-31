package importcc

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"dsh-go/pkg/storage"
)

// Options configures one import run.
type Options struct {
	// From names the source harness ("claude" today; "codex" is reserved).
	From string
	// ProjectsDir overrides the CC projects root (default ~/.claude/projects).
	ProjectsDir string
	// DataDir is the DSH sqlite data dir (default .dsh-data, like the CLI).
	DataDir string
	// Out receives the per-file result lines and the final summary (nil = discard).
	Out io.Writer
}

// ImportClaude performs one idempotent pass over a CC projects tree: every
// <enc-cwd>/*.jsonl file is converted and appended into the store unless a
// session with the same claude-import origin marker already exists.
func ImportClaude(opts Options) error {
	projectsDir := opts.ProjectsDir
	if projectsDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home for default --projects-dir: %w", err)
		}
		projectsDir = filepath.Join(home, ".claude", "projects")
	}
	return importClaude(projectsDir, opts.DataDir, opts.Out)
}

func importClaude(projectsDir, dataDir string, out io.Writer) error {
	absProjects, err := filepath.Abs(projectsDir)
	if err != nil {
		absProjects = projectsDir
	}
	projects, err := listProjectDirs(absProjects)
	if err != nil {
		return fmt.Errorf("unreadable projects dir %q: %w", projectsDir, err)
	}
	if len(projects) == 0 {
		fmt.Fprintf(out, "no project directories under %s\n", projectsDir)
		return nil
	}

	store, err := storage.OpenSqliteStore(dataDir)
	if err != nil {
		return fmt.Errorf("open DSH store %q: %w", dataDir, err)
	}
	defer store.Close()

	imported := map[string]bool{}
	if heads, lerr := store.ListSessions(); lerr == nil {
		for _, h := range heads {
			if uuid, ok := sourceUUIDOf(h.Origin); ok {
				imported[uuid] = true
				imported[h.ID] = true // duplicate markers by session id too
			}
		}
	}

	var files []string
	for _, p := range projects {
		jsonls, err := filepath.Glob(filepath.Join(p, "*.jsonl"))
		if err != nil {
			return fmt.Errorf("scan %q: %w", p, err)
		}
		files = append(files, jsonls...)
	}
	// A directory of jsonl session files passed directly (--projects-dir
	// already pointing at one <enc-cwd>) is a valid target too.
	direct, err := filepath.Glob(filepath.Join(absProjects, "*.jsonl"))
	if err != nil {
		return fmt.Errorf("scan %q: %w", absProjects, err)
	}
	files = dedupeSorted(append(files, direct...))

	stats := struct{ imported, already, empty, failed int }{}
	for _, path := range files {
		// The file name is the CC session uuid and is stable across runs, so
		// the idempotency check runs before any parsing work.
		fileUUID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if imported[fileUUID] {
			stats.already++
			fmt.Fprintf(out, "already=%s (imported before; skip)\n", fileUUID)
			continue
		}
		var converted *ConvertResult
		func() {
			f, oerr := os.Open(path)
			if oerr != nil {
				return
			}
			defer f.Close()
			converted, _ = ConvertClaudeJSONL(f, fileUUID)
		}()
		if converted == nil {
			stats.failed++
			fmt.Fprintf(out, "failed=%s (unreadable file)\n", fileUUID)
			continue
		}
		if len(converted.Envelopes) == 0 {
			stats.empty++
			fmt.Fprintf(out, "empty=%s lines=%d mappable=0 (skip)\n", converted.SourceID, converted.Lines)
			continue
		}
		header := converted.Header()
		if aerr := store.AppendEvents(header, converted.Envelopes); aerr != nil {
			stats.failed++
			fmt.Fprintf(out, "failed=%s (store append: %v)\n", converted.SourceID, aerr)
			continue
		}
		stats.imported++
		imported[converted.SourceID] = true
		fmt.Fprintf(out, "imported=%s events=%d skipped=%d sidechain=%d\n",
			converted.SourceID, len(converted.Envelopes), converted.Skipped, converted.Sidechain)
	}

	fmt.Fprintf(out, "import complete: files=%d imported=%d already=%d empty=%d failed=%d\n",
		len(files), stats.imported, stats.already, stats.empty, stats.failed)
	return nil
}

// listProjectDirs lists the immediate subdirectories of the CC projects root
// (the per-workspace "<encoded-cwd>" directories). A root that only holds
// .jsonl files (single-project invocation) is reported as itself.
func listProjectDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	return dirs, nil
}

func dedupeSorted(paths []string) []string {
	sort.Strings(paths)
	out := paths[:0]
	var prev string
	for _, p := range paths {
		if p == prev {
			continue
		}
		out = append(out, p)
		prev = p
	}
	return out
}

// Options.Run is the CLI entry point: validates --from and dispatches.
func (opts Options) Run() error {
	switch opts.From {
	case "claude":
		return ImportClaude(opts)
	case "codex":
		return fmt.Errorf("--from codex is reserved for the Codex importer (planned) and is not implemented yet")
	case "":
		return fmt.Errorf("--from is required for import (supported: claude)")
	default:
		return fmt.Errorf("unsupported --from %s (supported: claude; codex is reserved)", strconv.Quote(opts.From))
	}
}

// Run validates options and executes the import; the CLI turns the returned
// error into exit status 1.
func Run(opts Options) error {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	return opts.Run()
}
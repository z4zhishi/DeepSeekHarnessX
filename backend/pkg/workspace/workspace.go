// Package workspace provides the workspace-entity capability seam (upstream
// CK/packages/workspace). A workspace is a persistent folder-bound entity with
// a title and an ordered set of member sessions. The package also supplies a
// bounded directory-tree scanner so the gateway can back the frontend directory
// picker (workspace.list real directory tree) without walking the whole disk.
//
// The wire-facing RPCs (workspace.list / workspace.create) live in the gateway;
// this package is the backend state machine they drive.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrAlreadyExists is returned when Create is called for a path already under
// management.
var ErrAlreadyExists = errors.New("workspace: path already registered")

// ErrNotExist is returned when a workspace is addressed by an unknown id.
var ErrNotExist = errors.New("workspace: not found")

// Workspace is one managed workspace entity: a persistent folder with a title
// and an ordered set of session memberships (upstream title + membership).
type Workspace struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"createdAt"` // Unix milliseconds
	// Sessions is the ordered list of session IDs belonging to this workspace.
	Sessions []string `json:"sessions"`
}

// DirNode is one node of the directory tree served to the frontend picker.
// Children are present only for directories and are pruned to a bounded depth
// and count (see Manager.Scan defaults).
type DirNode struct {
	Name     string     `json:"name"`
	Path     string     `json:"path"`
	IsDir    bool       `json:"isDir"`
	Children []*DirNode `json:"children,omitempty"`
}

// ScanOptions bounds the directory scan so a huge tree cannot stall the picker.
type ScanOptions struct {
	// MaxDepth bounds recursion; 0 means the manager default (4).
	MaxDepth int
	// MaxEntries bounds the number of children enumerated per directory; 0
	// means the manager default (512).
	MaxEntries int
}

// Manager is the workspace backend: registration, creation, and directory-tree
// scanning. It is safe for concurrent use.
type Manager struct {
	mu           sync.RWMutex
	workspaces   []*Workspace
	scan         ScanOptions
	rootFallback string
}

// NewManager constructs an empty workspace manager. root is the default
// directory used by Scan when no workspace is addressed; when empty it falls
// back to the process working directory at scan time.
func NewManager(root string) *Manager {
	return &Manager{
		scan:         ScanOptions{MaxDepth: 4, MaxEntries: 512},
		rootFallback: root,
	}
}

// Add registers a pre-existing workspace directory without creating it on disk
// (used for roots injected at startup). It returns the registered entity.
func (m *Manager) Add(path string) (*Workspace, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace: %q is not a directory", abs)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.workspaces {
		if w.Path == abs {
			return w, ErrAlreadyExists
		}
	}
	ws := &Workspace{
		ID:        newID(abs),
		Path:      abs,
		Name:      filepath.Base(abs),
		Title:     filepath.Base(abs),
		CreatedAt: time.Now().UnixMilli(),
		Sessions:  []string{},
	}
	m.workspaces = append(m.workspaces, ws)
	return ws, nil
}

// Create makes a new workspace: it creates the directory on disk (when absent)
// and registers the resulting entity. It is the backend of workspace.create.
func (m *Manager) Create(path string) (*Workspace, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("workspace: %q exists and is not a directory", abs)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	} else {
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return nil, err
		}
	}
	return m.Add(abs)
}

// Get returns the workspace with the given id, or ErrNotExist.
func (m *Manager) Get(id string) (*Workspace, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, w := range m.workspaces {
		if w.ID == id {
			return w, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotExist, id)
}

// List returns every managed workspace, ordered by registration time (stable).
func (m *Manager) List() []*Workspace {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Workspace, 0, len(m.workspaces))
	for _, w := range m.workspaces {
		out = append(out, w)
	}
	return out
}

// SetTitle updates a workspace's display title.
func (m *Manager) SetTitle(id, title string) (*Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.workspaces {
		if w.ID == id {
			w.Title = title
			return w, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotExist, id)
}

// AddSession appends a session id to a workspace's ordered membership, unless
// already present.
func (m *Manager) AddSession(id, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.workspaces {
		if w.ID == id {
			for _, s := range w.Sessions {
				if s == sessionID {
					return nil
				}
			}
			w.Sessions = append(w.Sessions, sessionID)
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrNotExist, id)
}

// Scan returns the directory tree rooted at root. When root is empty, the
// first managed workspace (or the manager default/working directory) is used.
// The tree is bounded by the effective scan options.
func (m *Manager) Scan(root string, opts ScanOptions) (*DirNode, error) {
	if root == "" {
		root = m.firstPath()
	}
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace: %q is not a directory", abs)
	}

	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = m.scan.MaxDepth
	}
	maxEntries := opts.MaxEntries
	if maxEntries <= 0 {
		maxEntries = m.scan.MaxEntries
	}
	return scanDir(abs, 0, maxDepth, maxEntries)
}

// firstPath returns the path of the first managed workspace, or "" when none.
func (m *Manager) firstPath() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.workspaces) == 0 {
		return m.rootFallback
	}
	return m.workspaces[0].Path
}

// scanDir walks one directory level and recursively builds its subtree,
// bounded by maxDepth and maxEntries.
func scanDir(abs string, depth, maxDepth, maxEntries int) (*DirNode, error) {
	entries, err := os.ReadDir(abs)
	if err != nil {
		// A permission-denied or vanished child must not abort the whole tree.
		return &DirNode{Name: filepath.Base(abs), Path: abs, IsDir: true}, nil
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	var dirs, files []string
	for _, e := range entries {
		if len(dirs)+len(files) >= maxEntries {
			break
		}
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		} else {
			files = append(files, e.Name())
		}
	}

	node := &DirNode{Name: filepath.Base(abs), Path: abs, IsDir: true}
	if depth >= maxDepth {
		return node, nil
	}
	for _, d := range dirs {
		if len(node.Children) >= maxEntries {
			break
		}
		child, err := scanDir(filepath.Join(abs, d), depth+1, maxDepth, maxEntries)
		if err != nil {
			continue
		}
		node.Children = append(node.Children, child)
	}
	for _, f := range files {
		if len(node.Children) >= maxEntries {
			break
		}
		node.Children = append(node.Children, &DirNode{
			Name:  f,
			Path:  filepath.Join(abs, f),
			IsDir: false,
		})
	}
	return node, nil
}

// newID derives a stable, readable workspace id from a path.
func newID(abs string) string {
	return fmt.Sprintf("ws-%x", strings.NewReplacer("\\", "/", ":", "", "/", "").Replace(abs))
}

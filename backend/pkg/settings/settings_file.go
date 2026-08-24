package settings

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// FileStore persists a settings manager's document to a single YAML file.
// On Load it merges the on-disk document into the manager so registered
// namespaces resolve through user overrides; on Save it writes the full
// document back atomically.
type FileStore struct {
	path string
}

// NewFileStore returns a store bound to path (created when absent).
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

// Load reads the document file into the manager's in-memory doc. A missing
// or empty file leaves the document empty. On success it also materializes
// the file (0600) so describe reports hasDocument.
func (f *FileStore) Load(m *Manager) error {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			m.doc = map[string]any{}
			return nil
		}
		return err
	}
	var doc map[string]any
	if len(data) == 0 {
		doc = map[string]any{}
	} else if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	m.doc = normalize(doc)
	m.reload()
	return nil
}

// reload re-resolves every registered namespace against the freshly loaded
// document, firing onChange when a resolved value actually changed (deep-equal
// gated). Called after Load swaps the document.
func (m *Manager) reload() {
	for _, reg := range m.regs {
		prev := reg.resolved
		next := m.resolve(reg)
		if !deepEqual(prev, next) {
			reg.resolved = next
			if m.onChange != nil {
				m.onChange(reg.ns, reg.revision, next, prev)
			}
		}
	}
}

// Save writes the manager document to disk atomically (0600). A nil document
// is treated as empty. Absent parent directories are created.
func (f *FileStore) Save(m *Manager) error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(m.doc)
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}

// EnsureExists materializes the file if absent so editors always have a
// document to open.
func (f *FileStore) EnsureExists(m *Manager) error {
	if _, err := os.Stat(f.path); err == nil {
		return nil
	}
	return f.Save(m)
}

// Path returns the bound document path.
func (f *FileStore) Path() string { return f.path }

// normalize converts YAML-decoded maps (map[string]interface{}) into the
// canonical map[string]any shape nested throughout.
func normalize(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalizeValue(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			sk := ""
			if s, ok := k.(string); ok {
				sk = s
			} else {
				sk = fmt.Sprint(k)
			}
			out[sk] = normalizeValue(val)
		}
		return out
	}
	return map[string]any{}
}

func normalizeValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalizeValue(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			sk := ""
			if s, ok := k.(string); ok {
				sk = s
			} else {
				sk = fmt.Sprint(k)
			}
			out[sk] = normalizeValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = normalizeValue(t[i])
		}
		return out
	default:
		return v
	}
}

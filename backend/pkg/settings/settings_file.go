package settings

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

// FileStore persists a settings manager's document to a single YAML file.
// On Load it merges the on-disk document into the manager so registered
// namespaces resolve through user overrides; on Save it writes the full
// document back atomically.
//
// A document that exists but cannot be parsed poisons the store (upstream
// boot-fails-loud): Save then refuses to run so a later write can never
// replace the user's hand-edited file with an empty document.
type FileStore struct {
	path string

	mu       sync.Mutex // guards unusable
	unusable bool       // an on-disk document failed to parse; Save must refuse
}

// NewFileStore returns a store bound to path (created when absent).
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

// Load reads the document file into the manager's in-memory doc. A missing
// or empty file leaves the document empty. An unparsable file returns the
// error AND poisons the store: every later Save refuses to overwrite the
// unreadable document instead of destroying it.
func (f *FileStore) Load(m *Manager) error {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			f.setUnusable(false)
			m.loadDocument(map[string]any{})
			return nil
		}
		return err
	}
	var doc map[string]any
	if len(data) == 0 {
		doc = map[string]any{}
	} else if uerr := yaml.Unmarshal(data, &doc); uerr != nil {
		f.setUnusable(true)
		log.Printf("settings: %s cannot be parsed (%v); SAVING IS DISABLED to protect your file - "+
			"fix or remove it, then restart", f.path, uerr)
		return uerr
	}
	if doc == nil {
		doc = map[string]any{}
	}
	f.setUnusable(false)
	m.loadDocument(normalize(doc))
	return nil
}

func (f *FileStore) setUnusable(v bool) {
	f.mu.Lock()
	f.unusable = v
	f.mu.Unlock()
}

func (f *FileStore) isUnusable() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unusable
}

// reloadLocked re-resolves every registered namespace against the freshly
// loaded document, firing onChange when a resolved value actually changed
// (deep-equal gated). Called from loadDocument with m.mu held; revision
// advancement for changed raw sections also happens there.
func (m *Manager) reloadLocked() {
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

// Save writes the manager document to disk atomically (0600). It serializes
// the manager's latest published immutable snapshot, so it is safe to call
// concurrently with mutations — including from inside the persistence hook,
// where it observes the candidate being committed. Absent parent directories
// are created. A poisoned store (unparsable document seen at Load) refuses to
// write rather than destroy the user's file.
func (f *FileStore) Save(m *Manager) error {
	if f.isUnusable() {
		return fmt.Errorf("settings: refusing to overwrite %s: the existing file failed to parse earlier; fix or remove it first", f.path)
	}
	data, err := yaml.Marshal(m.publishedDocument())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
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

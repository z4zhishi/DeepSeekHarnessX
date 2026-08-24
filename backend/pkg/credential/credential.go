// Package credential provides the credential-reference seam (upstream
// CK/packages/credentials/credentials). A configuration carries a *reference*
// (an environment-variable name); consumers resolve it once per operation so a
// changed credential reaches the next operation without a restart.
//
// Resolution layers four sources, highest priority first (upstream
// provider-local env/file/project-env/user-env):
//
//  1. process environment (read-only, always wins)
//  2. $DSH_HOME/.credentials.yaml (writable store)
//  3. <cwd>/.env (project env, read-only)
//  4. $DSH_HOME/.env (user env, read-only)
//
// An empty stored value is absent everywhere — a blank never masquerades as a
// configured secret. set/unset persist to the writable layer and fire the
// changed callback; process-env and .env files are never written.
package credential

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Source names, mirroring upstream's provider source layer ids.
const (
	SourceEnv        = "env"         // process environment
	SourceFile       = "file"        // $DSH_HOME/.credentials.yaml
	SourceProjectEnv = "project-env" // <cwd>/.env
	SourceUserEnv    = "user-env"    // $DSH_HOME/.env
)

// refPattern mirrors upstream REF_PATTERN: a POSIX shell identifier.
var refPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Resolved is one credential value and the source layer that supplied it.
type Resolved struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}

// Info is source/writability facts safe for configuration surfaces — never the value.
type Info struct {
	Configured bool   `json:"configured"`
	Source     string `json:"source,omitempty"`
	Writable   bool   `json:"writable"`
}

// ErrInvalidRef is returned for a name outside the reference grammar.
var ErrInvalidRef = errors.New("credential: reference must match [A-Za-z_][A-Za-z0-9_]*")

// Manager resolves and stores credentials across the four layers. It is safe
// for concurrent use.
type Manager struct {
	mu      sync.RWMutex
	file    string // path of the writable .credentials.yaml
	creds   map[string]string
	envs    map[string]string // merged read-only .env layers (project + user-suffixed)
	changed func(ref string)
}

// Options configure a Manager.
type Options struct {
	// DSHHome is $DSH_HOME; when empty it falls back to the user home dir.
	DSHHome string
	// Cwd is the project directory for the project .env layer; when empty it
	// falls back to the process working directory.
	Cwd string
	// OnChanged, when non-nil, is invoked after a committed set/unset.
	OnChanged func(ref string)
}

// NewManager constructs a credential manager and loads all layers. Absent
// files are simply unconfigured layers, never an error.
func NewManager(opts Options) *Manager {
	home := opts.DSHHome
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	cwd := opts.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	return &Manager{
		file:    filepath.Join(home, ".credentials.yaml"),
		creds:   loadCredsFile(filepath.Join(home, ".credentials.yaml")),
		envs:    loadEnvFiles(home, cwd),
		changed: opts.OnChanged,
	}
}

// Resolve returns the current value of a reference and its source, or nil
// while unconfigured.
func (m *Manager) Resolve(ref string) (*Resolved, error) {
	if !IsRefName(ref) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidRef, ref)
	}
	if v := os.Getenv(ref); v != "" {
		return &Resolved{Value: v, Source: SourceEnv}, nil
	}
	m.mu.RLock()
	v, ok := m.creds[ref]
	m.mu.RUnlock()
	if ok && v != "" {
		return &Resolved{Value: v, Source: SourceFile}, nil
	}
	if v, ok := m.envs[ref]; ok && v != "" {
		return &Resolved{Value: v, Source: SourceProjectEnv}, nil
	}
	if v, ok := m.envs[ref+"_user"]; ok && v != "" {
		return &Resolved{Value: v, Source: SourceUserEnv}, nil
	}
	return nil, nil
}

// ResolveValue is the seam entry point returning a value string only (upstream
// resolve returns ResolvedCredential | undefined). It returns ("", nil) while
// unconfigured.
func (m *Manager) ResolveValue(ref string) (string, error) {
	r, err := m.Resolve(ref)
	if err != nil {
		return "", err
	}
	if r == nil {
		return "", nil
	}
	return r.Value, nil
}

// Describe reports configuration state and writability for a reference,
// never its value.
func (m *Manager) Describe(ref string) (Info, error) {
	r, err := m.Resolve(ref)
	if err != nil {
		return Info{}, err
	}
	if r == nil {
		return Info{Writable: true}, nil
	}
	// Writable only when the supplying layer is the writable file store.
	// A read-only source (env / project-env / user-env) shadows the value, so
	// a write there would never take effect.
	return Info{Configured: true, Source: r.Source, Writable: r.Source == SourceFile}, nil
}

// Set persists a value into the writable layer and notifies. It rejects an
// empty value and any write a read-only source would shadow.
func (m *Manager) Set(ref, value string) error {
	if !IsRefName(ref) {
		return fmt.Errorf("%w: %q", ErrInvalidRef, ref)
	}
	if value == "" {
		return fmt.Errorf("credential: refusing to store an empty value for %q (use Unset)", ref)
	}
	if r, err := m.Resolve(ref); err == nil && r != nil && r.Source != SourceFile {
		return fmt.Errorf("credential %q is shadowed by the read-only %s source; set is refused", ref, r.Source)
	}
	if err := os.MkdirAll(filepath.Dir(m.file), 0o700); err != nil {
		return err
	}
	m.mu.Lock()
	m.creds[ref] = value
	if err := m.saveLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	cb := m.changed
	m.mu.Unlock()
	if cb != nil {
		cb(ref)
	}
	return nil
}

// Unset removes a reference from the store (a no-op while absent). It rejects
// while a read-only source shadows the reference.
func (m *Manager) Unset(ref string) error {
	if !IsRefName(ref) {
		return fmt.Errorf("%w: %q", ErrInvalidRef, ref)
	}
	if r, err := m.Resolve(ref); err == nil && r != nil && r.Source != SourceFile {
		return fmt.Errorf("credential %q is shadowed by the read-only %s source; unset is refused", ref, r.Source)
	}
	m.mu.Lock()
	if _, exists := m.creds[ref]; !exists {
		m.mu.Unlock()
		return nil
	}
	delete(m.creds, ref)
	if err := m.saveLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	cb := m.changed
	m.mu.Unlock()
	if cb != nil {
		cb(ref)
	}
	return nil
}

// CredentialFile returns the writable credentials file path.
func (m *Manager) CredentialFile() string { return m.file }

// ListRefs returns every reference present in the writable store, values
// excluded. Absent store yields an empty list.
func (m *Manager) ListRefs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.creds))
	for ref := range m.creds {
		out = append(out, ref)
	}
	return out
}

// saveLocked persists the creds map atomically. Caller holds m.mu.
func (m *Manager) saveLocked() error {
	data, err := yaml.Marshal(m.creds)
	if err != nil {
		return err
	}
	tmp := m.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.file)
}

// IsRefName reports whether a string could name a reference at all.
func IsRefName(value string) bool {
	return value != "" && refPattern.MatchString(value)
}

// loadCredsFile reads the writable store into a map. Absent/unparseable files
// return an empty map (never an error).
func loadCredsFile(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var raw map[string]string
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return out
	}
	if raw != nil {
		out = raw
	}
	return out
}

// parseEnvFile parses a trivial .env (key=value) file: comments and blank
// lines skipped, optional surrounding quotes stripped, no expansion.
func parseEnvFile(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		out[key] = strings.Trim(strings.TrimSpace(val), `"'`)
	}
	return out
}

// loadEnvFiles merges the project and user .env layers. User entries are
// keyed with a "_user" suffix so Resolve can tell the two read-only layers
// apart (user-env is lower priority than project-env).
func loadEnvFiles(home, cwd string) map[string]string {
	out := map[string]string{}
	if cwd != "" {
		for k, v := range parseEnvFile(filepath.Join(cwd, ".env")) {
			out[k] = v
		}
	}
	if home != "" {
		for k, v := range parseEnvFile(filepath.Join(home, ".env")) {
			out[k+"_user"] = v
		}
	}
	return out
}

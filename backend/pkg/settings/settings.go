// Package settings provides the user-settings capability seam (upstream
// CK/packages/settings/settings). It owns namespace registration plus a single
// user-editable YAML document; each namespace layers schema defaults, the
// registrant's composition `base`, and the user document section, in that
// order. Changes are emitted through an OnChange callback gated by deep
// equality so a no-op write never notifies.
//
// The wire-facing RPCs (settings.describe / settings.mutate) live in the
// gateway; this package is the backend state machine they drive.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// nsPattern mirrors upstream NAMESPACE_PATTERN: lowercase kebab-case.
var nsPattern = "abcdefghijklmnopqrstuvwxyz0123456789"

// Namespace registration options beyond the schema.
type Options struct {
	// Base is the composition layer resolved below the user document section.
	Base map[string]any
	// Applies is the effect timing surfaced to configuration UIs; "live" or "restart".
	Applies string
	// Writable gates in-process mutation for this namespace.
	Writable bool
}

// Op is one path-addressed edit to a namespace's user section (upstream
// SettingsPathOp): op "set" carries a value, "unset" removes the path.
type Op struct {
	Op    string   `json:"op"`
	Path  []string `json:"path"`
	Value any      `json:"value,omitempty"`
}

// Mutation is the settings.mutate input shape.
type Mutation struct {
	NS  string `json:"ns"`
	Ops []Op   `json:"ops"`
}

// Descriptor is one registered namespace surfaced to configuration surfaces.
type Descriptor struct {
	NS       string          `json:"ns"`
	Schema   json.RawMessage `json:"schema"`
	Base     map[string]any  `json:"base,omitempty"`
	User     map[string]any  `json:"user,omitempty"`
	Revision int             `json:"revision"`
	Writable bool            `json:"writable"`
	Applies  string          `json:"applies,omitempty"`
}

// registration is one live namespace registration.
type registration struct {
	ns       string
	schema   json.RawMessage
	base     map[string]any
	applies  string
	writable bool
	resolved map[string]any
	revision int
}

// Manager is the settings backend: namespace registration, resolution,
// document mutation, and change notification. It is safe for concurrent use.
type Manager struct {
	path     string
	regs     map[string]*registration
	doc      map[string]any // ns -> user section
	onChange func(ns string, revision int, next, prev any)
	persist  func() error // optional: persists the document after a committed write
}

// NewManager constructs a settings manager. path is the user document file
// ($DSH_HOME/settings.yaml); onChange, when non-nil, is invoked after a
// committed change to a namespace's resolved value (deep-equal gated).
func NewManager(path string, onChange func(ns string, revision int, next, prev any)) *Manager {
	return &Manager{
		path:     path,
		regs:     map[string]*registration{},
		doc:      map[string]any{},
		onChange: onChange,
	}
}

// SetPersist installs a persistence callback invoked after every committed
// write (Mutate/Set). It lets the host flush the document to disk through its
// FileStore. A nil callback disables persistence.
func (m *Manager) SetPersist(fn func() error) {
	m.persist = fn
}

// persistLocked flushes the document through the installed callback. Caller
// holds m.mu (or is single-threaded during construction).
func (m *Manager) persistLocked() {
	if m.persist == nil {
		return
	}
	if err := m.persist(); err != nil {
		// Persistence is best-effort: a failed flush must not roll back the
		// in-memory commit, but it is surfaced so hosts can log it.
		fmt.Printf("settings: persist failed: %v\n", err)
	}
}

// ErrNamespaceNotRegistered is returned when a mutation names an unknown namespace.
var ErrNamespaceNotRegistered = errors.New("settings: namespace is not registered")

// ErrNamespaceInvalid reports a malformed namespace name.
var ErrNamespaceInvalid = errors.New("settings: namespace must be lowercase kebab-case")

// Register a namespace. Duplicate registration fails loud.
func (m *Manager) Register(ns string, schema json.RawMessage, opts Options) error {
	if !validNamespace(ns) {
		return fmt.Errorf("%w: %q", ErrNamespaceInvalid, ns)
	}
	if _, exists := m.regs[ns]; exists {
		return fmt.Errorf("settings namespace %q is already registered", ns)
	}
	reg := &registration{
		ns:       ns,
		schema:   schema,
		base:     cloneMap(opts.Base),
		applies:  opts.Applies,
		writable: opts.Writable,
	}
	if reg.applies == "" {
		reg.applies = "live"
	}
	reg.resolved = m.resolve(reg)
	reg.revision = revisionOf(reg)
	m.regs[ns] = reg
	return nil
}

// Get returns the resolved value for a registered namespace, or nil while
// unregistered.
func (m *Manager) Get(ns string) (any, error) {
	reg := m.regs[ns]
	if reg == nil {
		return nil, fmt.Errorf("%w: %s", ErrNamespaceNotRegistered, ns)
	}
	return reg.resolved, nil
}

// Describe returns one descriptor per registered namespace, in registration order.
func (m *Manager) Describe() []Descriptor {
	out := make([]Descriptor, 0, len(m.regs))
	for _, reg := range m.regs {
		d := Descriptor{
			NS:       reg.ns,
			Schema:   reg.schema,
			Base:     cloneMap(reg.base),
			Revision: reg.revision,
			Writable: reg.writable,
			Applies:  reg.applies,
		}
		if sec, ok := m.doc[reg.ns]; ok {
			if obj, ok := sec.(map[string]any); ok {
				d.User = cloneMap(obj)
			}
		}
		out = append(out, d)
	}
	return out
}

// Mutate applies ordered path edits to a namespace's user section, persists,
// and commits. It returns the namespace's new revision.
func (m *Manager) Mutate(ns string, ops []Op) (int, error) {
	reg := m.regs[ns]
	if reg == nil {
		return 0, fmt.Errorf("%w: %s", ErrNamespaceNotRegistered, ns)
	}
	if !reg.writable {
		return 0, fmt.Errorf("settings provider is read-only: %q cannot be updated", ns)
	}
	if len(ops) == 0 {
		return reg.revision, nil
	}
	for _, op := range ops {
		if op.Op != "set" && op.Op != "unset" {
			return 0, fmt.Errorf("settings mutate for %q ops must be {op:'set'|'unset', path}", ns)
		}
		if len(op.Path) == 0 || emptyPath(op.Path) {
			return 0, fmt.Errorf("settings mutate for %q op paths must be non-empty arrays of strings", ns)
		}
	}

	// Snapshot the current raw section and apply ops against it (never a
	// caller-owned object).
	nextSection, err := applyOps(m.rawSection(ns), ops)
	if err != nil {
		return 0, err
	}

	// Revision moves whenever the RAW section changed, so a field going from
	// inherited to overridden is observable to editors even when the resolved
	// value is unchanged.
	reg.revision += 1
	if m.doc == nil {
		m.doc = map[string]any{}
	}
	m.doc[ns] = nextSection

	// Resolve and notify only when the resolved value changed (deep-equal gate).
	prev := reg.resolved
	next := m.resolve(reg)
	if !deepEqual(prev, next) {
		reg.resolved = next
		if m.onChange != nil {
			m.onChange(ns, reg.revision, next, prev)
		}
	}
	m.persistLocked()
	return reg.revision, nil
}

// validNamespace reports whether ns is lowercase kebab-case (upstream
// NAMESPACE_PATTERN).
func validNamespace(ns string) bool {
	if ns == "" {
		return false
	}
	for _, c := range ns {
		if !strings.ContainsRune(nsPattern, c) && c != '-' {
			return false
		}
	}
	return true
}

// rawSection returns the namespace's raw user section or nil.
func (m *Manager) rawSection(ns string) map[string]any {
	sec, ok := m.doc[ns]
	if !ok {
		return nil
	}
	if obj, ok := sec.(map[string]any); ok {
		return obj
	}
	return nil
}

// resolve computes a namespace's resolved value: schema defaults, then base,
// then the user section.
func (m *Manager) resolve(reg *registration) map[string]any {
	out := schemaDefaults(reg.schema)
	out = mergeLayers(out, reg.base)
	out = mergeLayers(out, m.rawSection(reg.ns))
	return out
}

// revisionOf returns the current raw-section revision (0 before any write).
func revisionOf(reg *registration) int {
	return reg.revision
}

// applyOps applies ordered path ops to a detached section.
func applyOps(section map[string]any, ops []Op) (map[string]any, error) {
	out := cloneMap(section)
	if out == nil {
		out = map[string]any{}
	}
	for _, op := range ops {
		var err error
		out, err = applyPathOp(out, op)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// applyPathOp applies one op to a section, returning the next section.
func applyPathOp(section map[string]any, op Op) (map[string]any, error) {
	if len(op.Path) == 0 {
		if op.Op == "unset" {
			return map[string]any{}, nil
		}
		if !isPlainObject(op.Value) {
			return nil, fmt.Errorf("settings mutate: setting the section root requires a plain object")
		}
		return cloneMap(op.Value.(map[string]any)), nil
	}
	head := op.Path[0]
	if len(op.Path) == 1 {
		if op.Op == "unset" {
			delete(section, head)
			return section, nil
		}
		section[head] = cloneValue(op.Value)
		return section, nil
	}
	child, _ := section[head].(map[string]any)
	if child == nil {
		if op.Op == "unset" {
			return section, nil
		}
		child = map[string]any{}
	}
	nextChild, err := applyPathOp(child, Op{Op: op.Op, Path: op.Path[1:], Value: op.Value})
	if err != nil {
		return nil, err
	}
	section[head] = nextChild
	return section, nil
}

func isPlainObject(v any) bool {
	if v == nil {
		return false
	}
	_, ok := v.(map[string]any)
	return ok
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cloneMap(t)
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = cloneValue(t[i])
		}
		return out
	default:
		return v
	}
}

// mergeLayers layers `over` onto `under`: plain objects merge recursively,
// every other value replaces the lower layer wholesale.
func mergeLayers(under, over map[string]any) map[string]any {
	if under == nil {
		under = map[string]any{}
	}
	if over == nil {
		return under
	}
	for k, v := range over {
		if v == nil {
			continue
		}
		ov, ok := v.(map[string]any)
		uv, uok := under[k].(map[string]any)
		if ok && uok {
			under[k] = mergeLayers(uv, ov)
		} else {
			under[k] = cloneValue(v)
		}
	}
	return under
}

// schemaDefaults walks a JSON Schema's properties and collects `default` values.
func schemaDefaults(schema json.RawMessage) map[string]any {
	var s struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Default    any                        `json:"default"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return map[string]any{}
	}
	if s.Default != nil {
		if m, ok := s.Default.(map[string]any); ok {
			return cloneMap(m)
		}
	}
	out := map[string]any{}
	for name, raw := range s.Properties {
		var p struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Default    any                        `json:"default"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		if p.Default != nil {
			out[name] = cloneValue(p.Default)
		} else if len(p.Properties) > 0 {
			nested := schemaDefaults(raw)
			if len(nested) > 0 {
				out[name] = nested
			}
		}
	}
	return out
}

func emptyPath(parts []string) bool {
	for _, p := range parts {
		if p == "" {
			return true
		}
	}
	return false
}

// deepEqual reports structural equality over JSON-compatible values.
func deepEqual(a, b any) bool {
	if _, aIsMap := a.(map[string]any); aIsMap {
		return deepEqualMap(a, b)
	}
	if _, aIsSlice := a.([]any); aIsSlice {
		return deepEqualSlice(a, b)
	}
	if a == b {
		return true
	}
	return false
}

func deepEqualMap(a, b any) bool {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if !aok || !bok {
		return false
	}
	if len(am) != len(bm) {
		return false
	}
	for k, av := range am {
		bv, ok := bm[k]
		if !ok || !deepEqual(av, bv) {
			return false
		}
	}
	return true
}

func deepEqualSlice(a, b any) bool {
	al, aok := a.([]any)
	bl, bok := b.([]any)
	if !aok || !bok {
		return false
	}
	if len(al) != len(bl) {
		return false
	}
	for i := range al {
		if !deepEqual(al[i], bl[i]) {
			return false
		}
	}
	return true
}

// Set replaces a namespace's user section wholesale and persists. It returns
// the new revision. (Used by credential-free in-process consumers; the gateway
// path is Mutate.)
func (m *Manager) Set(ns string, section map[string]any) (int, error) {
	reg := m.regs[ns]
	if reg == nil {
		return 0, fmt.Errorf("%w: %s", ErrNamespaceNotRegistered, ns)
	}
	if !reg.writable {
		return 0, fmt.Errorf("settings provider is read-only: %q cannot be updated", ns)
	}
	prev := reg.resolved
	m.doc[ns] = cloneMap(section)
	reg.revision += 1
	next := m.resolve(reg)
	if !deepEqual(prev, next) {
		reg.resolved = next
		if m.onChange != nil {
			m.onChange(ns, reg.revision, next, prev)
		}
	}
	m.persistLocked()
	return reg.revision, nil
}

// DocumentExists reports whether a stored user section exists for any
// registered namespace. It backs settings.describe's hasDocument field.
func (m *Manager) DocumentExists() bool {
	return len(m.doc) > 0
}

// Package feedback is a lifecycle-bound, per-message rating+note sidecar
// store.
//
// It mirrors the semantics of the upstream `@deepseek-ai/dsh-message-feedback`
// service (CK/packages/feedback/message-feedback) as a self-contained Go
// store: rows are keyed by Session and each holds a list of immutable Item
// snapshots for assistant messages. Writes are compare-and-set on an opaque
// Version token so a stale client can never clobber a concurrent edit.
package feedback

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode"
)

// Rating is the closed judgment vocabulary for one assistant message.
type Rating string

const (
	// RatingLike is the positive judgment of the message.
	RatingLike Rating = "like"
	// RatingDislike is the negative judgment of the message.
	RatingDislike Rating = "dislike"
)

// Item is one current feedback value and its opaque mutation token.
// Values are immutable snapshots: callers may hold them freely.
type Item struct {
	// MessageID is the stable identity of the assistant message inside the
	// owning Session.
	MessageID string `json:"messageId"`
	// Rating is the overall like/dislike judgment.
	Rating Rating `json:"rating"`
	// Note is an optional explanation, preserved verbatim after validation.
	Note string `json:"note,omitempty"`
	// Version is an equality-only token replaced by every material mutation.
	Version string `json:"version"`
	// CreatedAt is the host-assigned creation time in Unix epoch milliseconds.
	CreatedAt int64 `json:"createdAt"`
	// UpdatedAt is the host-assigned time of the most recent material update.
	UpdatedAt int64 `json:"updatedAt"`
}

// Config carries deployment-varying policy. NewStore validates it at the
// configuration boundary so a bad value fails immediately rather than
// mid-operation.
type Config struct {
	// MaxNoteBytes is the maximum UTF-8 byte length accepted for one note.
	MaxNoteBytes int
}

// Sentinel errors surfaced to callers (and Phase 2's RPC mapping).
var (
	// ErrSessionNotFound is returned when no Session has any feedback yet.
	ErrSessionNotFound = errors.New("feedback: session-not-found")
	// ErrTargetNotFound is returned when the message id is unknown to the Session.
	ErrTargetNotFound = errors.New("feedback: target-not-found")
	// ErrNoteBlank is returned for a note with no non-whitespace character.
	ErrNoteBlank = errors.New("feedback: note-blank")
	// ErrVersionConflict is the class of a failed compare-and-set.
	ErrVersionConflict = errors.New("feedback: version-conflict")
)

// VersionConflictError carries the authoritative current item so a caller can
// diff and retry with a fresh token. Current is nil when no item exists.
type VersionConflictError struct {
	Current *Item
}

func (e *VersionConflictError) Error() string { return ErrVersionConflict.Error() }
func (e *VersionConflictError) Unwrap() error { return ErrVersionConflict }

// NoteTooLargeError reports the configured and actual UTF-8 byte lengths.
type NoteTooLargeError struct {
	MaxBytes    int
	ActualBytes int
}

func (e *NoteTooLargeError) Error() string {
	return fmt.Sprintf("feedback: note-too-large (max=%d actualBytes=%d)", e.MaxBytes, e.ActualBytes)
}

// row is the mutable per-Session item list. Stored Items are never mutated
// after insertion, so held Item values stay valid across later writes.
type row struct {
	items []Item
}

// Store is a thread-safe, in-memory, per-message feedback sidecar keyed by
// Session. It is sidecar bounded: it never creates or resumes an Agent or
// Session, and its lifespan is bounded by the owning process.
type Store struct {
	maxNoteBytes int
	mu           sync.RWMutex
	rows         map[string]*row // sessionID -> ordered feedback items
}

// NewStore validates the config and returns a ready Store.
func NewStore(cfg Config) (*Store, error) {
	if cfg.MaxNoteBytes < 1 {
		return nil, fmt.Errorf("feedback: maxNoteBytes must be a positive safe integer, got %d", cfg.MaxNoteBytes)
	}
	return &Store{
		maxNoteBytes: cfg.MaxNoteBytes,
		rows:         make(map[string]*row),
	}, nil
}

// List returns the current feedback items for one session in first-creation
// order. An absent session yields an empty slice. Each Item is an independent
// snapshot safe for caller mutation.
func (s *Store) List(sessionID string) []Item {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.rows[sessionID]
	if r == nil {
		return []Item{}
	}
	out := make([]Item, len(r.items))
	copy(out, r.items)
	return out
}

// PutRequest identifies the target message, the desired value, and the
// caller's observed version.
type PutRequest struct {
	// SessionID is the persisted Session that owns the target message.
	SessionID string
	// MessageID is the target assistant-message identity.
	MessageID string
	// Rating is the desired judgment.
	Rating Rating
	// Note is an optional non-blank explanation, kept verbatim.
	Note string
	// HasNote distinguishes an unset note from an explicitly empty one.
	HasNote bool
	// IfVersion is the observed item version; "" requires that no item exists.
	IfVersion string
}

// Put creates or replaces feedback for one message. A matching no-op (same
// rating and note at the same version) returns the stored item without
// changing its revision. A version mismatch returns VersionConflictError with
// the authoritative current item.
func (s *Store) Put(req PutRequest) (Item, error) {
	if req.Rating != RatingLike && req.Rating != RatingDislike {
		return Item{}, fmt.Errorf("feedback: invalid rating %q", req.Rating)
	}
	if req.HasNote {
		if err := s.validateNote(req.Note); err != nil {
			return Item{}, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	r := s.rows[req.SessionID]
	if r == nil {
		r = &row{}
		s.rows[req.SessionID] = r
	}
	idx := indexOf(r.items, req.MessageID)
	var existing *Item
	if idx != -1 {
		existing = &r.items[idx]
	}
	currentVersion := ""
	if existing != nil {
		currentVersion = existing.Version
	}
	if req.IfVersion != currentVersion {
		return Item{}, &VersionConflictError{Current: cloneItem(existing)}
	}
	if existing != nil && existing.Rating == req.Rating && existingNote(existing, req) == req.Note {
		// Idempotent no-op: preserve the stored revision.
		return *cloneItem(existing), nil
	}

	now := time.Now().UnixMilli()
	item := Item{
		MessageID: req.MessageID,
		Rating:    req.Rating,
		Version:   newVersion(),
		CreatedAt: createdAtOf(existing, now),
		UpdatedAt: updatedAtOf(existing, now),
	}
	if req.HasNote {
		item.Note = req.Note
	}
	if idx == -1 {
		r.items = append(r.items, item)
	} else {
		r.items[idx] = item
	}
	return item, nil
}

// DeleteRequest identifies the value to remove and the caller's observed
// version.
type DeleteRequest struct {
	// SessionID is the persisted Session that owns the sidecar.
	SessionID string
	// MessageID whose feedback should be absent after this operation.
	MessageID string
	// IfVersion is the observed item version; ignored when already absent.
	IfVersion string
}

// Delete removes one feedback item. Absence is successful regardless of the
// supplied version; an existing item requires an exact match, else
// VersionConflictError carries the current item.
func (s *Store) Delete(req DeleteRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := s.rows[req.SessionID]
	if r == nil {
		return nil
	}
	idx := indexOf(r.items, req.MessageID)
	if idx == -1 {
		return nil
	}
	if req.IfVersion != r.items[idx].Version {
		return &VersionConflictError{Current: cloneItem(&r.items[idx])}
	}
	r.items = append(r.items[:idx], r.items[idx+1:]...)
	if len(r.items) == 0 {
		delete(s.rows, req.SessionID)
	}
	return nil
}

// validateNote enforces non-blank and the configured UTF-8 byte bound.
func (s *Store) validateNote(note string) error {
	if isBlank(note) {
		return ErrNoteBlank
	}
	if n := len([]byte(note)); n > s.maxNoteBytes {
		return &NoteTooLargeError{MaxBytes: s.maxNoteBytes, ActualBytes: n}
	}
	return nil
}

func isBlank(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func indexOf(items []Item, messageID string) int {
	for i, it := range items {
		if it.MessageID == messageID {
			return i
		}
	}
	return -1
}

func cloneItem(item *Item) *Item {
	if item == nil {
		return nil
	}
	c := *item
	return &c
}

// existingNote returns the note the request should compare against an
// existing item: an absent request matches an item that has no note.
func existingNote(item *Item, req PutRequest) string {
	if !req.HasNote {
		return ""
	}
	return item.Note
}

func createdAtOf(existing *Item, now int64) int64 {
	if existing != nil {
		return existing.CreatedAt
	}
	return now
}

func updatedAtOf(existing *Item, now int64) int64 {
	if existing == nil {
		return now
	}
	if now > existing.UpdatedAt {
		return now
	}
	return existing.UpdatedAt
}

// newVersion returns a unique opaque equality token for one mutation.
func newVersion() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x-%d", b, time.Now().UnixNano())
}

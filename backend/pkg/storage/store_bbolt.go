package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"dsh-go/pkg/session"
	bolt "go.etcd.io/bbolt"
)

var (
	bucketSessions   = []byte("sessions")
	bucketEvents     = []byte("events")
	bucketWorkspaces = []byte("workspaces")
	bucketSettings   = []byte("settings")
)

// BboltStore provides an embedded, zero-CGo B+Tree storage engine with mmap zero-copy reads.
type BboltStore struct {
	db *bolt.DB
}

// OpenBboltStore initializes or opens the bbolt database file.
func OpenBboltStore(dataDir string) (*BboltStore, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	dbPath := filepath.Join(dataDir, "dsh.bbolt")
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{
		Timeout: 2 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open bbolt db: %w", err)
	}

	// Initialize buckets
	err = db.Update(func(tx *bolt.Tx) error {
		for _, bName := range [][]byte{bucketSessions, bucketEvents, bucketWorkspaces, bucketSettings} {
			if _, err := tx.CreateBucketIfNotExists(bName); err != nil {
				return fmt.Errorf("failed to create bucket %s: %w", string(bName), err)
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}

	return &BboltStore{db: db}, nil
}

// PutSession saves or updates session header metadata.
func (s *BboltStore) PutSession(header *session.SessionHeader) error {
	data, err := json.Marshal(header)
	if err != nil {
		return err
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSessions)
		return b.Put([]byte(header.ID), data)
	})
}

// GetSession retrieves a session header by ID.
func (s *BboltStore) GetSession(sessionID string) (*session.SessionHeader, error) {
	var header session.SessionHeader
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSessions)
		val := b.Get([]byte(sessionID))
		if val == nil {
			return fmt.Errorf("session not found: %s", sessionID)
		}
		return json.Unmarshal(val, &header)
	})
	if err != nil {
		return nil, err
	}
	return &header, nil
}

// ListSessions lists all stored sessions.
func (s *BboltStore) ListSessions() ([]session.SessionHeader, error) {
	var list []session.SessionHeader
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSessions)
		return b.ForEach(func(k, v []byte) error {
			var h session.SessionHeader
			if err := json.Unmarshal(v, &h); err == nil {
				list = append(list, h)
			}
			return nil
		})
	})
	return list, err
}

// AppendEvents inserts session envelopes into the database.
func (s *BboltStore) AppendEvents(meta *session.SessionHeader, envelopes []*session.SessionEnvelope) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketEvents)
		for _, env := range envelopes {
			key := []byte(fmt.Sprintf("%s:%010d", meta.ID, env.Seq))
			val, err := json.Marshal(env)
			if err != nil {
				return err
			}
			if err := b.Put(key, val); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetEvents retrieves all events for a session starting from `fromSeq`.
func (s *BboltStore) GetEvents(sessionID string, fromSeq int) ([]session.SessionEnvelope, error) {
	var list []session.SessionEnvelope
	prefix := []byte(sessionID + ":")
	startKey := []byte(fmt.Sprintf("%s:%010d", sessionID, fromSeq))

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketEvents)
		c := b.Cursor()

		for k, v := c.Seek(startKey); k != nil && hasPrefix(k, prefix); k, v = c.Next() {
			var env session.SessionEnvelope
			if err := json.Unmarshal(v, &env); err == nil {
				list = append(list, env)
			}
		}
		return nil
	})

	return list, err
}

// Close closes the database cleanly.
func (s *BboltStore) Close() error {
	return s.db.Close()
}

func hasPrefix(s, prefix []byte) bool {
	return len(s) >= len(prefix) && string(s[:len(prefix)]) == string(prefix)
}

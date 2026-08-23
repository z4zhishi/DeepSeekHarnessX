package storage

import "dsh-go/pkg/session"

// SqliteGatewayStore adapts SqliteStore to the gateway's storage surface
// (value-typed ListSessions). The schema-17 SQLite backend is the default
// persistence engine; bbolt remains available through the legacy flag.
type SqliteGatewayStore struct {
	*SqliteStore
}

// ListSessions returns all stored session headers as values.
// AppendEvents is inherited from *SqliteStore via embedding and satisfies the
// gateway SessionStore surface with the same appendBatch semantics.

// ListSessions returns all stored session headers as values.
func (s *SqliteGatewayStore) ListSessions() ([]session.SessionHeader, error) {
	rows, err := s.SqliteStore.ListSessions()
	if err != nil {
		return nil, err
	}
	out := make([]session.SessionHeader, 0, len(rows))
	for _, r := range rows {
		if r != nil {
			out = append(out, *r)
		}
	}
	return out, nil
}

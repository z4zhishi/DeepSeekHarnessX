package storage

// store_sqlite.go
//
// SQLite persistence store, bit-exact compatible with the upstream
// DeepSeek-Harness SCHEMA_VERSION=17 physical layout (the "packed chunk-row"
// format of `session-persistence-sqlite`). This file ports the upstream seam to
// pure-Go:
//
//   - storage medium: one SQLite database (`sessions.db`) owning the same tables
//     upstream materializes (`persistence_state`, `sessions`, `events`) under
//     `PRAGMA user_version = 17` and `PRAGMA application_id = 1146308688`.
//   - chunk packing: a run of >= 3 consecutive, same-block `assistant/chunk`
//     deltas is stored as ONE physical row tagged `text-chunks` /
//     `reasoning-chunks` / `tool-call-chunks` whose `data` column is the packed
//     JSON `{ "turn", "step", "index", "dt": number[], "texts"|"args": string[] }`,
//     with `ignorable = 0` as the packed sentinel and `source_event_seqs`,
//     `surface_op` NULL. Decoding reconstructs every logical event's exact
//     `seq`, `time`, `turn`, `step`, and chunk 鈥?bit-for-bit equal to what the
//     upstream codec (`codec.ts`) expands.
//
// The store also ships a read-side `.jsonl.zstd` reader that decodes the
// upstream concatenated-Zstandard-frame session artifact: one checksummed frame
// per durable batch, the first frame carrying the `type:"session"` header line.
//
// Engine: pure-Go `modernc.org/sqlite` 鈥?no cgo, no system libsqlite3, no
// runtime C compiler. This project builds with CGO_ENABLED=0 (verified) and
// targets a portable Windows executable, so modernc is the compatible choice;
// it ships SQLite as transpiled Go and registers the "sqlite" driver.
//
// Durability: `PRAGMA journal_mode = WAL` + `PRAGMA synchronous = NORMAL`
// (SQLite value 1 — SQLite's recommended WAL posture), `PRAGMA foreign_keys = ON`,
// `PRAGMA trusted_schema = OFF`, `PRAGMA mmap_size = 0`. Every mutation runs
// inside a single IMMEDIATE transaction (modernc `_txlock=immediate` DSN) so a
// competing writer cannot interleave; COMMIT is the atomicity boundary and
// fsyncs batch at WAL checkpoints (the former synchronous=FULL flush-synced
// every event append).

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	sqlite "modernc.org/sqlite"

	"dsh-go/pkg/session"
)

// SCHEMA_VERSION mirrors upstream `session-persistence-sqlite`
// `SCHEMA_VERSION = 17` 鈥?the physical-record layout version in `user_version`.
const SCHEMA_VERSION = 17

// SESSION_PERSISTENCE_SQLITE_APPLICATION_ID is the reserved application id for
// DeepSeek-Harness SQLite session databases (`0x44534850`). Rejects a database
// that is a different application's SQLite file even at user_version 17.
const SESSION_PERSISTENCE_SQLITE_APPLICATION_ID = int64(0x44534850)

// Codec sizing, mirroring upstream `codec.ts` constants.
const (
	MIN_PACKED_ROW_MEMBERS = 3
	MAX_PACKED_ROW_MEMBERS = 1_024
	MAX_PACKED_DATA_BYTES  = 1_048_576
	// zstdDataThresholdBytes: values at/above this are stored as a compressed
	// zstd frame BLOB; smaller values stay as SQLite text.
	zstdDataThresholdBytes = 4_096
	// PACKED_ROW_SENTINEL is the `ignorable` column value marking a packed row.
	PACKED_ROW_SENTINEL = 0
)

// sqliteBusyTimeoutMs mirrors upstream DEFAULT_BUSY_TIMEOUT_MS.
const sqliteBusyTimeoutMs = 5000

var uuidRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ---------------------------------------------------------------------------
// Physical row and packed-record shapes
// ---------------------------------------------------------------------------

// EventRow is one physical event row. Data may be plaintext UTF-8 JSON
// (`IsText`) or a zstd-compressed JSON BLOB, matching the upstream `ANY` data
// column binding that a decoder treats as text-or-blob.
type EventRow struct {
	Seq             int
	Type            string
	Time            int64
	Data            []byte
	IsText          bool
	SourceEventSeqs []byte // nil when NULL; zig-zag varint sequence otherwise
	SurfaceOp       []byte // nil when NULL; JSON-encoded SurfaceOp otherwise
	HasIgnorable    bool   // false when the column is NULL (scalar, absent)
	Ignorable       int    // 0 = packed sentinel, 1 = ignorable true
}

// ChunkRow is one schema-17 packed physical record (mirrors upstream ChunkRow).
type ChunkRow struct {
	Type  string  `json:"type"` // "text-chunks" | "reasoning-chunks" | "tool-call-chunks"
	Seq0  int     `json:"seq0"`
	Time0 int64   `json:"time0"`
	Data  RunData `json:"data"`
}

// RunData is the packed delta payload; exactly one of Texts/Args is present.
type RunData struct {
	Turn  int      `json:"turn"`
	Step  int      `json:"step"`
	Index int      `json:"index"`
	DT    []int64  `json:"dt"`              // dt[i] = time[i+1] - time[i]
	Texts []string `json:"texts,omitempty"` // text / reasoning runs
	Args  []string `json:"args,omitempty"`  // tool-call runs
	ID    string   `json:"id,omitempty"`    // tool-call only
	Name  string   `json:"name,omitempty"`  // tool-call only, optional
}

// StorageRecord is either a scalar logical event or a packed ChunkRow.
type StorageRecord = any

func packedDataBytes(row ChunkRow) int {
	b, _ := json.Marshal(row.Data)
	return len(b)
}

// ---------------------------------------------------------------------------
// Store: open, configure, schema
// ---------------------------------------------------------------------------

// SqliteStore is the pure-Go, schema-17 SQLite persistence store.
type SqliteStore struct {
	path string
	db   *sql.DB

	// storeIdentity mirrors upstream's source-qualified identity so revisions
	// distinguish this medium. Set at open.
	storeIdentity string

	// schemaVerifiedOnce guards the once-per-store mutation recheck below:
	// schema ownership is fully verified at open (configureDatabase →
	// validateRequiredSchemaTx) and the pool pins ONE connection, so a second
	// full sqlite_schema + PRAGMA user_version scan inside every write
	// transaction re-proved what this process already proved (a 21-event turn
	// ran 21 identical scans). The per-batch application_id pragma tripwire
	// (see appendBatch) stays as the cheap sentinel for an alien writer.
	schemaVerified bool
}

// OpenSqliteStore opens (or creates) the schema-17 SQLite database under
// dataDir. Returns an error for any schema-version / application-id mismatch.
func OpenSqliteStore(dataDir string) (*SqliteStore, error) {
	s := &SqliteStore{}
	if err := s.init(dataDir); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SqliteStore) init(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}
	dbPath := filepath.Join(dataDir, "sessions.db")
	s.path = dbPath
	// Every pool connection carries the same PRAGMAs via the driver's
	// `_pragma` DSN parameters (per-connection settings must not depend on
	// which pooled connection a query lands on). Transactions stay default
	// (deferred): writes explicitly BEGIN IMMEDIATE, reads take the normal
	// deferred snapshot like upstream.
	dsn := fmt.Sprintf("file:%s?_busy_timeout=%d&_pragma=trusted_schema%%3dOFF&_pragma=mmap_size%%3d0&_pragma=foreign_keys%%3dON",
		dbPath, sqliteBusyTimeoutMs)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("failed to open sqlite db: %w", err)
	}
	// A single connection is the deterministic deployment: SQLite's
	// trusted_schema/mmap_size/synchronous/foreign_keys/busy_timeout are
	// per-connection settings and upstream assumes one writer.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s.db = db

	if err := s.configureDatabase(); err != nil {
		db.Close()
		return fmt.Errorf("sqlite open sequence failed: %w", err)
	}
	if err := s.loadStoreIdentity(); err != nil {
		db.Close()
		return fmt.Errorf("failed to read store identity: %w", err)
	}
	return nil
}

// configureDatabase mirrors the upstream open sequence: connection-security
// PRAGMAs with read-back verification, ownership checks, transactional
// initialization with exact schema-set validation, WAL selection (with busy
// retry), and durability PRAGMAs.
func (s *SqliteStore) configureDatabase() error {
	db := s.db
	// Per-connection security PRAGMAs also ride the DSN `_pragma` list; verify
	// them by read-back exactly like upstream configureConnectionSecurity.
	if trustedSchema, err := pragmaInt(db, "PRAGMA trusted_schema"); err != nil {
		return err
	} else if trustedSchema != 0 {
		return fmt.Errorf("session database at %q retained trusted_schema=%d, expected 0", s.path, trustedSchema)
	}
	if mmapSize, err := pragmaInt(db, "PRAGMA mmap_size"); err != nil {
		return err
	} else if mmapSize != 0 {
		return fmt.Errorf("session database at %q retained mmap_size=%d, expected 0", s.path, mmapSize)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("failed to enable foreign keys: %w", err)
	}

	// Initialization and ownership validation run inside one IMMEDIATE
	// transaction so a crash can never leave a half-created schema stamped as
	// versioned (the upstream initializeDatabase-in-begin-immediate shape).
	tx, err := s.beginImmediate()
	if err != nil {
		return fmt.Errorf("failed to begin initialization transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	onDisk, err := pragmaIntTx(tx, "PRAGMA user_version")
	if err != nil {
		return err
	}
	applicationID, err := pragmaIntTx(tx, "PRAGMA application_id")
	if err != nil {
		return err
	}
	var objectCount int64
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'`,
	).Scan(&objectCount); err != nil {
		return fmt.Errorf("failed to count schema objects: %w", err)
	}

	if onDisk == 0 && (applicationID != 0 || objectCount > 0) {
		return fmt.Errorf("session database at %q has an unversioned schema or application identity", s.path)
	}
	if onDisk != 0 && onDisk != SCHEMA_VERSION {
		return fmt.Errorf("session database at %q has schema version %d, incompatible with this build (%d)",
			s.path, onDisk, SCHEMA_VERSION)
	}
	if onDisk != 0 && applicationID != SESSION_PERSISTENCE_SQLITE_APPLICATION_ID {
		return fmt.Errorf("session database at %q has application id %d, expected %d",
			s.path, applicationID, SESSION_PERSISTENCE_SQLITE_APPLICATION_ID)
	}
	if onDisk == 0 {
		if err := initializeDatabaseTx(tx); err != nil {
			return err
		}
	}
	if err := validateRequiredSchemaTx(tx, s.path); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit schema initialization/validation: %w", err)
	}

	if err := s.selectJournalMode(); err != nil {
		return err
	}
	// WAL + synchronous=NORMAL is SQLite's recommended WAL posture: transactions
	// keep full atomicity and WAL durability (fsyncs move to WAL checkpoints,
	// which stream over batched appends), while avoiding a per-commit device
	// flush. The former synchronous=FULL paid one fsync PER EVENT on spinning
	// disks (a 21-delta turn = 21 × ~40ms syncs ≈ 0.9s of pure flush); power
	// loss now bounds loss to the WAL window since the last checkpoint, the
	// upstream harness design accepts for streaming agent logs.
	if _, err := db.Exec("PRAGMA synchronous = NORMAL"); err != nil {
		return fmt.Errorf("failed to set synchronous NORMAL: %w", err)
	}
	var synchronous int64
	if err := db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		return fmt.Errorf("failed to read synchronous: %w", err)
	}
	if synchronous != 1 {
		return fmt.Errorf("session database at %q retained synchronous=%d, expected NORMAL (1)", s.path, synchronous)
	}
	return nil
}

// sqliteBusyError reports whether err is SQLite's busy result code (errcode 5).
func sqliteBusyError(err error) bool {
	var sqlErr *sqlite.Error
	return errors.As(err, &sqlErr) && sqlErr.Code() == 5
}

func (s *SqliteStore) selectJournalMode() error {
	deadline := time.Now().Add(time.Duration(sqliteBusyTimeoutMs) * time.Millisecond)
	for {
		_, err := s.db.Exec("PRAGMA journal_mode = WAL")
		if err == nil {
			break
		}
		if !sqliteBusyError(err) || !time.Now().Before(deadline) {
			return fmt.Errorf("failed to select journal mode WAL: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("failed to read journal mode: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(mode), "wal") {
		return fmt.Errorf("session database at %q selected journal mode %q, expected WAL", s.path, mode)
	}
	return nil
}

// initializeDatabaseDDL is the canonical schema-17 DDL (byte-equal to the
// upstream resources/sql/schema.sql), shared by initialization and by the
// in-memory reference that defines the accepted object set.
const initializeDatabaseDDL = `
CREATE TABLE persistence_state (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  store_id  TEXT NOT NULL
) STRICT;

CREATE TABLE sessions (
  id               TEXT PRIMARY KEY,
  version          INTEGER NOT NULL,
  created_at       INTEGER NOT NULL,
  cwd              TEXT,
  parent_session   TEXT,
  seed_length      INTEGER,
  origin           TEXT,
  delegation_depth INTEGER,
  agent_preset     TEXT,
  incarnation      TEXT NOT NULL,
  revision         INTEGER NOT NULL
) STRICT;

CREATE TABLE events (
  session_id        TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  seq               INTEGER NOT NULL,
  type              TEXT NOT NULL,
  time              INTEGER NOT NULL,
  data              ANY NOT NULL,
  source_event_seqs ANY,
  surface_op        TEXT,
  ignorable         INTEGER CHECK (ignorable IS NULL OR ignorable IN (0, 1)),
  PRIMARY KEY (session_id, seq)
) STRICT;`

// initializeDatabaseTx creates the canonical three-table STRICT schema inside
// the caller's transaction; the version stamps assert the layout is complete
// before the transaction commits.
func initializeDatabaseTx(tx *sqliteTx) error {
	if _, err := tx.Exec(initializeDatabaseDDL); err != nil {
		return fmt.Errorf("failed to create schema: %w", err)
	}
	if _, err := tx.Exec(
		"INSERT INTO persistence_state (singleton, store_id) VALUES (1, ?)", newUUIDString()); err != nil {
		return fmt.Errorf("failed to insert store identity: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA application_id = %d", SESSION_PERSISTENCE_SQLITE_APPLICATION_ID)); err != nil {
		return fmt.Errorf("failed to stamp application id: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", SCHEMA_VERSION)); err != nil {
		return fmt.Errorf("failed to stamp schema version: %w", err)
	}
	return nil
}

// schemaObject is one row of the canonical schema-17 object set
// (upstream `select-schema-objects`).
type schemaObject struct {
	Type string
	Name string
	Tbl  string
	SQL  string
}

// normalizeSql collapses whitespace runs, mirroring upstream normalizeSql.
func normalizeSql(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

// schemaObjectsOf reads the (type, name, tbl_name, whitespace-normalized sql)
// tuple set of a database's user-visible schema objects.
func schemaObjectsOf(q interface {
	Query(query string, args ...any) (*sql.Rows, error)
}) ([]schemaObject, error) {
	rows, err := q.Query(
		`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*' ORDER BY type, name`)
	if err != nil {
		return nil, fmt.Errorf("failed to read schema objects: %w", err)
	}
	defer rows.Close()
	var out []schemaObject
	for rows.Next() {
		var o schemaObject
		if err := rows.Scan(&o.Type, &o.Name, &o.Tbl, &o.SQL); err != nil {
			return nil, fmt.Errorf("failed to scan schema object row: %w", err)
		}
		o.SQL = normalizeSql(o.SQL)
		out = append(out, o)
	}
	return out, rows.Err()
}

var (
	canonicalSchemaOnce sync.Once
	canonicalSchema     []schemaObject
)

// requiredSchemaObjects derives the exact schema-17 object set from an
// in-memory reference database built with the same DDL (the upstream
// expectedSchema approach): whatever a freshly initialized database exposes
// is precisely what this build accepts.
func requiredSchemaObjects() ([]schemaObject, error) {
	canonicalSchemaOnce.Do(func() {
		ref, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			panic("storage: cannot open in-memory schema reference: " + err.Error())
		}
		defer ref.Close()
		if _, err := ref.Exec(initializeDatabaseDDL); err != nil {
			panic("storage: schema reference init failed: " + err.Error())
		}
		if _, err := ref.Exec(
			"INSERT INTO persistence_state (singleton, store_id) VALUES (1, ?)", newUUIDString()); err != nil {
			panic("storage: schema reference insert failed: " + err.Error())
		}
		objs, err := schemaObjectsOf(ref)
		if err != nil {
			panic("storage: schema reference read failed: " + err.Error())
		}
		canonicalSchema = objs
	})
	return canonicalSchema, nil
}

// validateRequiredSchemaTx requires the durable schema object set (type, name,
// tbl_name, whitespace-normalized sql) to equal the canonical schema-17 set
// exactly — a database with an extra unrelated table would be rejected by
// upstream readers, so this build refuses it too.
func validateRequiredSchemaTx(tx *sqliteTx, path string) error {
	got, err := schemaObjectsOf(tx)
	if err != nil {
		return err
	}
	want, err := requiredSchemaObjects()
	if err != nil {
		return err
	}
	if len(got) != len(want) {
		return fmt.Errorf("session database at %q does not contain the required schema objects (%d objects, want %d)",
			path, len(got), len(want))
	}
	for i, o := range got {
		w := want[i]
		if o.Type != w.Type || o.Name != w.Name || o.Tbl != w.Tbl || o.SQL != w.SQL {
			return fmt.Errorf("session database at %q does not contain the required schema objects (mismatch at %s %q)",
				path, o.Type, o.Name)
		}
	}
	return nil
}

func pragmaInt(db *sql.DB, pragma string) (int64, error) {
	var v int64
	if err := db.QueryRow(pragma).Scan(&v); err != nil {
		return 0, fmt.Errorf("failed to read %s: %w", pragma, err)
	}
	return v, nil
}

func pragmaIntTx(tx *sqliteTx, pragma string) (int64, error) {
	var v int64
	if err := tx.QueryRow(pragma).Scan(&v); err != nil {
		return 0, fmt.Errorf("failed to read %s: %w", pragma, err)
	}
	return v, nil
}

func (s *SqliteStore) loadStoreIdentity() error {
	var storeID string
	err := s.db.QueryRow(
		"SELECT store_id FROM persistence_state WHERE singleton = 1").Scan(&storeID)
	if err != nil {
		return fmt.Errorf("session database at %q has no valid store identity: %w", s.path, err)
	}
	if !uuidRegex.MatchString(storeID) {
		return fmt.Errorf("session database at %q has invalid store_id", s.path)
	}
	abs, err := filepath.Abs(s.path)
	if err != nil {
		abs = s.path
	}
	s.storeIdentity = fmt.Sprintf("file:%s:store:%s", filepath.ToSlash(abs), storeID)
	return nil
}

// Close closes the database cleanly.
func (s *SqliteStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Store interface: loadStored / readStoredRevision / loadStoredFrom
// ---------------------------------------------------------------------------

// StoredPrefix mirrors upstream StoredPrefix: metadata, contiguous event
// prefix, source-qualified revision, and an optional torn-tail marker.
type StoredPrefix struct {
	Meta       *session.SessionHeader
	Events     []*session.SessionEnvelope
	Revision   string
	TornMarker *int
}

// StoredSuffix is the result of a seek-capable suffix read.
type StoredSuffix struct {
	Meta   *session.SessionHeader
	Events []*session.SessionEnvelope
}

func (s *SqliteStore) loadStored(id string) (*StoredPrefix, error) {
	row, err := s.sessionRow(id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	rows, err := s.selectEvents(id, 0)
	if err != nil {
		return nil, err
	}
	preserved, tornFrom, err := scanRows(rows, 0)
	if err != nil {
		return nil, err
	}
	return &StoredPrefix{
		Meta:       rowToMeta(row),
		Events:     preserved,
		TornMarker: tornFrom,
		Revision:   s.revisionOf(row),
	}, nil
}

func (s *SqliteStore) readStoredRevision(id string) (string, error) {
	row, err := s.sessionRow(id)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", nil
	}
	return s.revisionOf(row), nil
}

func (s *SqliteStore) loadStoredFrom(id string, fromSeq int) (*StoredSuffix, error) {
	row, err := s.sessionRow(id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	base, eventRows, err := s.physicalSpanFrom(id, fromSeq)
	if err != nil {
		return nil, err
	}
	preserved, _, err := scanRows(eventRows, base)
	if err != nil {
		return nil, err
	}
	var filtered []*session.SessionEnvelope
	for _, ev := range preserved {
		if ev.Seq >= fromSeq {
			filtered = append(filtered, ev)
		}
	}
	return &StoredSuffix{Meta: rowToMeta(row), Events: filtered}, nil
}

// LoadStoredFrom is the exported seek-capable suffix read.
func (s *SqliteStore) LoadStoredFrom(id string, fromSeq int) (*StoredSuffix, error) {
	return s.loadStoredFrom(id, fromSeq)
}

// ---------------------------------------------------------------------------
// Store interface: appendBatch / commitRepair
// ---------------------------------------------------------------------------

// appendBatch durably appends a contiguous batch, materializing the session
// first when !materialized. Materialize + first event batch commit ATOMICALLY.
// AppendEvents is the public append entry: it writes one contiguous event
// batch under the session metadata row (upstream `appendBatch` semantics,
// non-materialized). It is the write surface used by gateways and agents.
func (s *SqliteStore) AppendEvents(meta *session.SessionHeader, events []*session.SessionEnvelope) error {
	return s.appendBatch(meta, events, false)
}
// validateSchemaForMutation rechecks schema ownership inside the caller's
// mutation transaction (upstream validateSchemaForMutation): another writer may
// have changed the application identity, schema objects, or version since open.
func validateSchemaForMutation(tx *sqliteTx, path string) error {
	applicationID, err := pragmaIntTx(tx, "PRAGMA application_id")
	if err != nil {
		return err
	}
	if applicationID != SESSION_PERSISTENCE_SQLITE_APPLICATION_ID {
		return fmt.Errorf("session database application id changed before mutation (expected %d, got %d)",
			SESSION_PERSISTENCE_SQLITE_APPLICATION_ID, applicationID)
	}
	got, err := schemaObjectsOf(tx)
	if err != nil {
		return err
	}
	want, err := requiredSchemaObjects()
	if err != nil {
		return err
	}
	if len(got) != len(want) {
		return fmt.Errorf("session database at %q does not contain the required schema objects (%d objects, want %d)",
			path, len(got), len(want))
	}
	for i, o := range got {
		w := want[i]
		if o.Type != w.Type || o.Name != w.Name || o.Tbl != w.Tbl || o.SQL != w.SQL {
			return fmt.Errorf("session database at %q does not contain the required schema objects (mismatch at %s %q)",
				path, o.Type, o.Name)
		}
	}
	version, err := pragmaIntTx(tx, "PRAGMA user_version")
	if err != nil {
		return err
	}
	if version != SCHEMA_VERSION {
		return fmt.Errorf("session database schema changed before mutation (expected %d, got %d)",
			SCHEMA_VERSION, version)
	}
	return nil
}

func (s *SqliteStore) appendBatch(meta *session.SessionHeader, events []*session.SessionEnvelope, isMaterialized bool) error {
	if len(events) == 0 {
		return nil
	}
	// Writes take an explicit IMMEDIATE (write-reservation) transaction; the
	// pool-wide default stays deferred so reads keep upstream's snapshot shape.
	tx, err := s.beginImmediate()
	if err != nil {
		return fmt.Errorf("failed to begin immediate transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// Schema ownership: verified once per store lifetime (open + first
	// mutation), then only the 1-int application_id pragma re-runs per batch
	// as an alien-writer tripwire. DSHX assumes a single writer per database
	// (SetMaxOpenConns(1)); the full per-mutation recheck re-scanned
	// sqlite_schema on every event append.
	switch {
	case !s.schemaVerified:
		if err := validateSchemaForMutation(tx, s.path); err != nil {
			return err
		}
		s.schemaVerified = true
	default:
		appID, err := pragmaIntTx(tx, "PRAGMA application_id")
		if err != nil {
			return err
		}
		if appID != SESSION_PERSISTENCE_SQLITE_APPLICATION_ID {
			return fmt.Errorf("session database application id changed before mutation (expected %d, got %d)",
				SESSION_PERSISTENCE_SQLITE_APPLICATION_ID, appID)
		}
	}

	if !isMaterialized {
		if err := writeSessionRowTx(tx, meta); err != nil {
			return err
		}
	} else if err := ensureSessionRowTx(tx, meta); err != nil {
		return err
	}

	last, err := logicalLastEventTx(tx, meta.ID)
	if err != nil {
		return err
	}
	expected := 0
	if last != nil {
		expected = last.Seq + 1
	}
	if events[0].Seq != expected {
		return fmt.Errorf("session %s append starts at seq %d, stored next seq is %d",
			meta.ID, events[0].Seq, expected)
	}

	for _, rec := range packChunkRuns(events) {
		if err := insertRecordTx(tx, meta.ID, bindRecord(rec)); err != nil {
			return err
		}
	}
	if err := incrementRevisionTx(tx, meta.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// commitRepair truncates a torn tail (when tornMarker != nil) and appends
// synthetic closers (when any). Mirrors the coordinator's crash-repair path.
func (s *SqliteStore) commitRepair(meta *session.SessionHeader, tornMarker *int, closers []*session.SessionEnvelope) error {
	if tornMarker == nil && len(closers) == 0 {
		return nil
	}
	tx, err := s.beginImmediate()
	if err != nil {
		return fmt.Errorf("failed to begin immediate transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck

	if err := validateSchemaForMutation(tx, s.path); err != nil {
		return err
	}
	// Repair only ever applies to a known session: a missing metadata row is
	// an error, never a fresh materialization (upstream store.ts:211).
	row, err := sessionRowTx(tx, meta.ID)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("session %s metadata row is missing", meta.ID)
	}
	current, tornFrom, err := scanAllRowsTx(tx, meta.ID)
	if err != nil {
		return err
	}
	if tornMarker != nil {
		if tornFrom == nil || *tornFrom != *tornMarker {
			return fmt.Errorf("session %s repair is stale: physical tail no longer starts at seq %d",
				meta.ID, *tornMarker)
		}
		if err := deleteEventsFromTx(tx, meta.ID, *tornMarker); err != nil {
			return err
		}
	} else if tornFrom != nil {
		return fmt.Errorf("session %s repair omitted current torn tail at seq %d", meta.ID, *tornFrom)
	}
	if len(closers) > 0 {
		expected := 0
		if len(current) > 0 {
			expected = current[len(current)-1].Seq + 1
		}
		if closers[0].Seq != expected {
			return fmt.Errorf("session %s repair is stale: closer starts at seq %d, stored next seq is %d",
				meta.ID, closers[0].Seq, expected)
		}
		for _, closer := range closers {
			if err := insertRecordTx(tx, meta.ID, bindRecord(closer)); err != nil {
				return err
			}
		}
	}
	if err := incrementRevisionTx(tx, meta.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// PutSession saves or updates a session header (gateway compatibility
// surface; persists the metadata row exactly as appendBatch does for
// non-materialized sessions).
func (s *SqliteStore) PutSession(header *session.SessionHeader) error {
	tx, err := s.beginImmediate()
	if err != nil {
		return fmt.Errorf("failed to begin immediate transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if err := validateSchemaForMutation(tx, s.path); err != nil {
		return err
	}
	if err := writeSessionRowTx(tx, header); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteSession removes a session header and its events (best-effort).
func (s *SqliteStore) DeleteSession(sessionID string) error {
	tx, err := s.beginImmediate()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM events WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE id = ?`, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// GetSession retrieves a session header by id.
func (s *SqliteStore) GetSession(sessionID string) (*session.SessionHeader, error) {
	row, err := s.sessionRow(sessionID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	return rowToMeta(row), nil
}

// GetEvents retrieves all events for a session starting from `fromSeq` in
// logical seq order (scalar + expanded packed rows). A fromSeq inside a packed
// row's span resolves through the packed predecessor so no covered events are
// skipped (same physicalSpanFrom discipline as loadStoredFrom).
func (s *SqliteStore) GetEvents(sessionID string, fromSeq int) ([]session.SessionEnvelope, error) {
	base, rows, err := s.physicalSpanFrom(sessionID, fromSeq)
	if err != nil {
		return nil, err
	}
	preserved, tornFrom, err := scanRows(rows, base)
	if err != nil {
		return nil, err
	}
	_ = tornFrom // GetEvents is a read surface: torn tails simply stop the prefix.
	var out []session.SessionEnvelope
	for _, env := range preserved {
		if env.Seq >= fromSeq {
			out = append(out, *env)
		}
	}
	return out, nil
}

// ListSessions returns all stored (materialized) session headers.
func (s *SqliteStore) ListSessions() ([]*session.SessionHeader, error) {
	rows, err := s.sessionRowsAll()
	if err != nil {
		return nil, err
	}
	out := make([]*session.SessionHeader, 0, len(rows))
	for _, r := range rows {
		out = append(out, rowToMeta(r))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Session row mapping
// ---------------------------------------------------------------------------

type sessionRow struct {
	ID              string
	Version         int
	CreatedAt       int64
	Cwd             *string
	ParentSession   *string
	SeedLength      *int64
	Origin          *string
	DelegationDepth *int64
	AgentPreset     *string
	Incarnation     string
	Revision        int64
}

func (s *SqliteStore) sessionRow(id string) (*sessionRow, error) {
	row := s.db.QueryRow(`
SELECT id, version, created_at, cwd, parent_session, seed_length, origin,
       delegation_depth, agent_preset, incarnation, revision
FROM sessions WHERE id = ?`, id)
	var r sessionRow
	err := row.Scan(&r.ID, &r.Version, &r.CreatedAt, &r.Cwd, &r.ParentSession,
		&r.SeedLength, &r.Origin, &r.DelegationDepth, &r.AgentPreset, &r.Incarnation, &r.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load session %s: %w", id, err)
	}
	return &r, nil
}

func (s *SqliteStore) sessionRowsAll() ([]*sessionRow, error) {
	rows, err := s.db.Query(`
SELECT id, version, created_at, cwd, parent_session, seed_length, origin,
       delegation_depth, agent_preset, incarnation, revision FROM sessions`)
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}
	defer rows.Close()
	var out []*sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.ID, &r.Version, &r.CreatedAt, &r.Cwd, &r.ParentSession,
			&r.SeedLength, &r.Origin, &r.DelegationDepth, &r.AgentPreset, &r.Incarnation, &r.Revision); err != nil {
			return nil, fmt.Errorf("failed to scan session row: %w", err)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func writeSessionRowTx(tx *sqliteTx, meta *session.SessionHeader) error {
	_, err := tx.Exec(`
INSERT INTO sessions
  (id, version, created_at, cwd, parent_session, seed_length, origin,
   delegation_depth, agent_preset, incarnation, revision)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
ON CONFLICT(id) DO UPDATE SET
  version = excluded.version,
  created_at = excluded.created_at,
  cwd = excluded.cwd,
  parent_session = excluded.parent_session,
  seed_length = excluded.seed_length,
  origin = excluded.origin,
  delegation_depth = excluded.delegation_depth,
  agent_preset = excluded.agent_preset`,
		meta.ID, meta.Version, meta.CreatedAt,
		nullable(meta.Cwd), nullable(meta.ParentSession), nullableInt(meta.SeedLength),
		nullable(meta.Origin), nullableInt(meta.DelegationDepth), nullable(meta.AgentPreset),
		newUUIDString())
	if err != nil {
		return fmt.Errorf("failed to write session %s: %w", meta.ID, err)
	}
	return nil
}

// beginImmediate opens a write-reservation (IMMEDIATE) transaction on a single
// pinned connection, the Go equivalent of upstream `BEGIN IMMEDIATE`. database/
// sql transactions cannot nest a raw BEGIN, so the lock is taken with Exec on
// the connection and finished with Commit/Rollback; with SetMaxOpenConns(1)
// this reserves the write lock exactly like upstream. Reads use the pool's
// default deferred transactions instead.
type sqliteTx struct {
	conn   *sql.Conn
	owned  bool // true when the caller must finish the transaction
	closed bool
}

func (t *sqliteTx) Exec(query string, args ...any) (sql.Result, error) {
	return t.conn.ExecContext(context.Background(), query, args...)
}

func (t *sqliteTx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.conn.QueryContext(context.Background(), query, args...)
}

func (t *sqliteTx) QueryRow(query string, args ...any) *sql.Row {
	return t.conn.QueryRowContext(context.Background(), query, args...)
}

func (t *sqliteTx) Commit() error {
	if !t.owned || t.closed {
		return nil
	}
	t.closed = true
	_, err := t.Exec("COMMIT")
	cerr := t.conn.Close()
	if err != nil {
		return err
	}
	return cerr
}

func (t *sqliteTx) Rollback() error {
	if !t.owned || t.closed {
		return nil
	}
	t.closed = true
	_, err := t.Exec("ROLLBACK")
	cerr := t.conn.Close()
	if err != nil {
		return err
	}
	return cerr
}

func (s *SqliteStore) beginImmediate() (*sqliteTx, error) {
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return nil, err
	}
	tx := &sqliteTx{conn: conn}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, err
	}
	tx.owned = true
	return tx, nil
}

// ensureSessionRowTx verifies the session metadata row exists for a
// materialized append; unlike the non-materialized upsert it never creates a
// row — appending to an unknown session is an error (upstream relies on
// incrementRevision's changes!=1 to fail here).
func ensureSessionRowTx(tx *sqliteTx, meta *session.SessionHeader) error {
	row, err := sessionRowTx(tx, meta.ID)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("session %s metadata row is missing", meta.ID)
	}
	return nil
}

// sessionRowTx reads one session metadata row inside a transaction
// (nil when absent).
func sessionRowTx(tx *sqliteTx, id string) (*sessionRow, error) {
	var r sessionRow
	err := tx.QueryRow(`
SELECT id, version, created_at, cwd, parent_session, seed_length, origin,
       delegation_depth, agent_preset, incarnation, revision
FROM sessions WHERE id = ?`, id).Scan(&r.ID, &r.Version, &r.CreatedAt, &r.Cwd, &r.ParentSession,
		&r.SeedLength, &r.Origin, &r.DelegationDepth, &r.AgentPreset, &r.Incarnation, &r.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load session %s: %w", id, err)
	}
	return &r, nil
}

func incrementRevisionTx(tx *sqliteTx, id string) error {
	res, err := tx.Exec("UPDATE sessions SET revision = revision + 1 WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to increment revision for %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("session %s metadata row is missing", id)
	}
	return nil
}

func deleteEventsFromTx(tx *sqliteTx, id string, fromSeq int) error {
	if _, err := tx.Exec("DELETE FROM events WHERE session_id = ? AND seq >= ?", id, fromSeq); err != nil {
		return fmt.Errorf("failed to truncate session %s at seq %d: %w", id, fromSeq, err)
	}
	return nil
}

func (s *SqliteStore) revisionOf(row *sessionRow) string {
	return fmt.Sprintf("%s:incarnation:%s:revision:%d", s.storeIdentity, row.Incarnation, row.Revision)
}

func rowToMeta(r *sessionRow) *session.SessionHeader {
	h := &session.SessionHeader{Version: r.Version, ID: r.ID, CreatedAt: r.CreatedAt}
	if r.Cwd != nil {
		h.Cwd = *r.Cwd
	}
	if r.ParentSession != nil {
		h.ParentSession = *r.ParentSession
	}
	if r.SeedLength != nil {
		h.SeedLength = int(*r.SeedLength)
	}
	if r.Origin != nil {
		h.Origin = *r.Origin
	}
	if r.DelegationDepth != nil {
		h.DelegationDepth = int(*r.DelegationDepth)
	}
	if r.AgentPreset != nil {
		h.AgentPreset = *r.AgentPreset
	}
	return h
}

// ---------------------------------------------------------------------------
// Event row selection
// ---------------------------------------------------------------------------

func (s *SqliteStore) selectEvents(id string, fromSeq int) ([]*EventRow, error) {
	query := `
SELECT seq, type, time, data, source_event_seqs, surface_op, ignorable
FROM events WHERE session_id = ? ORDER BY seq`
	args := []any{id}
	if fromSeq != 0 {
		query = `
SELECT seq, type, time, data, source_event_seqs, surface_op, ignorable
FROM events WHERE session_id = ? AND seq >= ? ORDER BY seq`
		args = append(args, fromSeq)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to select events for %s: %w", id, err)
	}
	defer rows.Close()
	return scanEventRows(rows)
}

func scanEventRows(rows *sql.Rows) ([]*EventRow, error) {
	var out []*EventRow
	for rows.Next() {
		var r EventRow
		var data any
		var src, surf sql.NullString
		var ign sql.NullInt64
		if err := rows.Scan(&r.Seq, &r.Type, &r.Time, &data, &src, &surf, &ign); err != nil {
			return nil, fmt.Errorf("failed to scan event row: %w", err)
		}
		switch v := data.(type) {
		case string:
			r.Data = []byte(v)
			r.IsText = true
		case []byte:
			r.Data = v
		default:
			return nil, fmt.Errorf("unexpected data column type %T", data)
		}
		if src.Valid {
			r.SourceEventSeqs = []byte(src.String)
		}
		if surf.Valid {
			r.SurfaceOp = []byte(surf.String)
		}
		if ign.Valid {
			r.HasIgnorable = true
			r.Ignorable = int(ign.Int64)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// physicalSpanFrom selects the bounded physical span that may represent
// fromSeq (a packed predecessor may start earlier than fromSeq).
func (s *SqliteStore) physicalSpanFrom(id string, fromSeq int) (int, []*EventRow, error) {
	floor := fromSeq - MAX_PACKED_ROW_MEMBERS + 1
	if floor < 0 {
		floor = 0
	}
	predecessors, err := s.queryPackedPredecessors(id, floor, fromSeq)
	if err != nil {
		return 0, nil, err
	}
	base := fromSeq
	for _, pred := range predecessors {
		events, err := decodeRow(pred)
		if err != nil {
			if pred.Seq < base {
				base = pred.Seq
			}
			continue
		}
		if len(events) > 0 && events[len(events)-1].Seq >= fromSeq && pred.Seq < base {
			base = pred.Seq
		}
	}
	rows, err := s.selectEvents(id, base)
	if err != nil {
		return 0, nil, err
	}
	return base, rows, nil
}

func (s *SqliteStore) queryPackedPredecessors(id string, floor, fromSeq int) ([]*EventRow, error) {
	rows, err := s.db.Query(`
SELECT seq, type, time, data, source_event_seqs, surface_op, ignorable
FROM events
WHERE session_id = ? AND seq >= ? AND seq < ?
  AND type IN ('text-chunks', 'reasoning-chunks', 'tool-call-chunks')
  AND ignorable = 0
ORDER BY seq`, id, floor, fromSeq)
	if err != nil {
		return nil, fmt.Errorf("failed to select packed predecessors: %w", err)
	}
	defer rows.Close()
	return scanEventRows(rows)
}

// logicalLastEventTx returns the last preserved logical event, or nil if empty.
func logicalLastEventTx(tx *sqliteTx, id string) (*session.SessionEnvelope, error) {
	rows, err := tx.Query(`
SELECT seq, type, time, data, source_event_seqs, surface_op, ignorable
FROM events WHERE session_id = ? ORDER BY seq DESC LIMIT 2`, id)
	if err != nil {
		return nil, err
	}
	tail, err := scanEventRows(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
		tail[i], tail[j] = tail[j], tail[i]
	}
	if len(tail) == 0 {
		return nil, nil
	}
	floor := tail[0].Seq - MAX_PACKED_ROW_MEMBERS + 1
	if floor < 0 {
		floor = 0
	}
	span, err := tx.Query(`
SELECT seq, type, time, data, source_event_seqs, surface_op, ignorable
FROM events WHERE session_id = ? AND seq >= ? ORDER BY seq`, id, floor)
	if err != nil {
		return nil, err
	}
	spanRows, err := scanEventRows(span)
	span.Close()
	if err != nil {
		return nil, err
	}
	preserved, tornFrom, err := scanRows(spanRows, spanRows[0].Seq)
	if err != nil {
		return nil, err
	}
	if tornFrom != nil {
		return nil, fmt.Errorf("session %s has an invalid physical tail at seq %d", id, *tornFrom)
	}
	if len(preserved) == 0 {
		return nil, nil
	}
	return preserved[len(preserved)-1], nil
}

func scanAllRowsTx(tx *sqliteTx, id string) ([]*session.SessionEnvelope, *int, error) {
	rows, err := tx.Query(`
SELECT seq, type, time, data, source_event_seqs, surface_op, ignorable
FROM events WHERE session_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, nil, err
	}
	eventRows, err := scanEventRows(rows)
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	return scanRows(eventRows, 0)
}

func insertRecordTx(tx *sqliteTx, id string, rec BoundRecord) error {
	_, err := tx.Exec(`
INSERT INTO events
  (session_id, seq, type, time, data, source_event_seqs, surface_op, ignorable)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, rec.Seq, rec.Type, rec.Time, rec.Data, rec.SourceEventSeqs, rec.SurfaceOp, rec.Ignorable)
	if err != nil {
		return fmt.Errorf("failed to insert event for %s: %w", id, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// BoundRecord: SQLite column values for one physical insert
// ---------------------------------------------------------------------------

type BoundRecord struct {
	Seq             int
	Type            string
	Time            int64
	Data            any // string (text) or []byte (zstd blob)
	SourceEventSeqs any // nil or []byte
	SurfaceOp       any // nil or []byte (JSON)
	Ignorable       any // nil, 0 (packed), or 1
}

func bindRecord(record StorageRecord) BoundRecord {
	if cr, ok := record.(ChunkRow); ok {
		data, _ := json.Marshal(cr.Data)
		return BoundRecord{
			Seq:       cr.Seq0,
			Type:      cr.Type,
			Time:      cr.Time0,
			Data:      encodeData(data),
			Ignorable: PACKED_ROW_SENTINEL,
		}
	}
	ev := record.(*session.SessionEnvelope)
	br := BoundRecord{
		Seq:  ev.Seq,
		Type: ev.Type,
		Time: ev.Time,
		Data: encodeData(ev.Data),
	}
	if ev.Ignorable {
		br.Ignorable = 1
	}
	if len(ev.SourceEventSeqs) > 0 {
		br.SourceEventSeqs = encodeSourceEventSeqs(ev.SourceEventSeqs)
	}
	if ev.SurfaceOp != nil {
		serialized, err := json.Marshal(ev.SurfaceOp)
		if err != nil {
			panic("invalid surface op: " + err.Error())
		}
		// events.surface_op is a STRICT TEXT column: SQLite rejects a BLOB
		// binding there (3091), and upstream stores JSON.stringify(surfaceOp).
		br.SurfaceOp = string(serialized)
	}
	return br
}

// encodeData: small values stay SQLite text; larger values are zstd-compressed
// into a BLOB when the compressed form is smaller.
func encodeData(serialized []byte) any {
	if len(serialized) < zstdDataThresholdBytes {
		return string(serialized)
	}
	if compressed, err := zstdEncode(serialized); err == nil && len(compressed) < len(serialized) {
		return compressed
	}
	return string(serialized)
}

// zstdEncPool reuses zstd encoders across write batches. The previous shape
// (zstd.NewWriter per call) arms one worker per GOMAXPROCS on first EncodeAll:
// on a 48-core host that measured 63.7 MB TotalAlloc per 64 KB encode
// (TestZstdPooledCost in pkg/agent) — freed immediately, but churning the heap
// arena and OS working set on every oversized event / jsonl frame write, a
// measurable share of the transient memory the first-session verdict saw.
// EncodeAll is goroutine-safe per encoder (workers are drawn from an internal
// channel and returned), so a small pooled set bounds the churn to ~105 KB per
// write. Frame output is byte-identical to the per-call shape for the same
// input regardless of encoder concurrency (pinned by
// TestZstdEncoderPooledByteIdentity below) — the schema-17 data-column bytes
// the upstream decoder reads do not change.
var zstdEncPool = sync.Pool{
	New: func() any {
		enc, err := zstd.NewWriter(nil,
			zstd.WithEncoderCRC(true), zstd.WithEncoderConcurrency(2))
		if err != nil {
			return nil
		}
		return enc
	},
}

func zstdEncode(data []byte) ([]byte, error) {
	if e, ok := zstdEncPool.Get().(*zstd.Encoder); ok && e != nil {
		defer zstdEncPool.Put(e)
		return e.EncodeAll(data, nil), nil
	}
	// Pool construction failed (not reachable with these options; kept so the
	// fallback matches the historical per-call shape exactly).
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderCRC(true), zstd.WithEncoderConcurrency(2))
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(data, nil), nil
}

// ---------------------------------------------------------------------------
// Chunk codec: pack / unpack
// ---------------------------------------------------------------------------

// jsonKeySet decodes a JSON object into its key set (nil when the value is not
// an object).
func jsonKeySet(raw json.RawMessage) map[string]bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil
	}
	keys := make(map[string]bool, len(obj))
	for k := range obj {
		keys[k] = true
	}
	return keys
}

// hasExactKeys reports whether keys is exactly the given whitelist.
func hasExactKeys(keys map[string]bool, want ...string) bool {
	if keys == nil || len(keys) != len(want) {
		return false
	}
	for _, w := range want {
		if !keys[w] {
			return false
		}
	}
	return true
}

// classifyDelta returns the delta kind for an assistant/chunk event, or "".
// Eligibility mirrors the upstream exact-keys whitelist (codec.ts classify):
// the envelope must be exactly {type,seq,time,data} — any ignorable flag,
// surfaceOp, sourceEventSeqs, or unknown extension makes the event ineligible
// so it is stored verbatim instead of lossily packed — data must be exactly
// {turn,step,chunk}, and the chunk must match one of the three delta variants'
// precise key sets. Unrecognized shapes are never packed; packing can only
// lose compression, never data.
func classifyDelta(env *session.SessionEnvelope) string {
	if env.Type != session.EventAssistantChunk {
		return ""
	}
	if env.Ignorable || env.SurfaceOp != nil || len(env.SourceEventSeqs) > 0 {
		return ""
	}
	if env.Seq < 0 {
		return ""
	}
	var data struct {
		Turn  *json.Number    `json:"turn"`
		Step  *json.Number    `json:"step"`
		Chunk json.RawMessage `json:"chunk"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return ""
	}
	dataKeys := jsonKeySet(env.Data)
	if !hasExactKeys(dataKeys, "turn", "step", "chunk") {
		return ""
	}
	if data.Turn == nil || data.Step == nil {
		return ""
	}
	chunkKeys := jsonKeySet(data.Chunk)
	if chunkKeys == nil {
		return ""
	}
	var chunk struct {
		Type    string  `json:"type"`
		Index   *int    `json:"index"`
		Text    *string `json:"text"`
		ID      *string `json:"id"`
		Name    *string `json:"name"`
		ArgDelt *string `json:"argumentsDelta"`
	}
	if err := json.Unmarshal(data.Chunk, &chunk); err != nil {
		return ""
	}
	switch chunk.Type {
	case "text-delta", "reasoning-delta":
		if hasExactKeys(chunkKeys, "type", "index", "text") && chunk.Index != nil && chunk.Text != nil {
			return chunk.Type
		}
	case "tool-call-delta":
		exact := hasExactKeys(chunkKeys, "type", "index", "id", "argumentsDelta")
		withName := hasExactKeys(chunkKeys, "type", "index", "id", "name", "argumentsDelta")
		if (exact || withName) && chunk.Index != nil && chunk.ID != nil && chunk.ArgDelt != nil && (!withName || chunk.Name != nil) {
			return chunk.Type
		}
	}
	return ""
}

// deltaFields extracts the packed chunk sub-shape for a recognized delta event.
func deltaFields(env *session.SessionEnvelope) (kind string, turn, step, index int, id, name, text, argDelt string) {
	if env.Type != session.EventAssistantChunk {
		return
	}
	var data struct {
		Turn  int             `json:"turn"`
		Step  int             `json:"step"`
		Chunk json.RawMessage `json:"chunk"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return
	}
	var chunk struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Text    string `json:"text"`
		ID      string `json:"id"`
		Name    string `json:"name"`
		ArgDelt string `json:"argumentsDelta"`
	}
	if err := json.Unmarshal(data.Chunk, &chunk); err != nil {
		return
	}
	kind, turn, step, index = chunk.Type, data.Turn, data.Step, chunk.Index
	switch chunk.Type {
	case "text-delta", "reasoning-delta":
		text = chunk.Text
	case "tool-call-delta":
		id, name, argDelt = chunk.ID, chunk.Name, chunk.ArgDelt
	}
	return
}

// continuesDelta reports whether next continues the current delta run.
func continuesDelta(kind string, prev, next *session.SessionEnvelope) bool {
	if next.Seq != prev.Seq+1 {
		return false
	}
	pk, pt, ps, pi, pid, pname, _, _ := deltaFields(prev)
	nk, nt, ns, ni, nid, nname, _, _ := deltaFields(next)
	if nk != kind || nk != pk {
		return false
	}
	if nt != pt || ns != ps || ni != pi {
		return false
	}
	if kind != "tool-call-delta" {
		return true
	}
	if pid != nid {
		return false
	}
	if (pname == "") != (nname == "") {
		return false
	}
	return pname == nname
}

// chunkTagOfKind maps a delta kind to its packed physical row tag.
func chunkTagOfKind(kind string) string {
	switch kind {
	case "text-delta":
		return "text-chunks"
	case "reasoning-delta":
		return "reasoning-chunks"
	case "tool-call-delta":
		return "tool-call-chunks"
	}
	return ""
}

func buildRow(kind string, run []*session.SessionEnvelope) ChunkRow {
	first := run[0]
	_, turn, step, index, id, name, _, _ := deltaFields(first)
	dt := make([]int64, len(run)-1)
	for i := 0; i < len(run)-1; i++ {
		dt[i] = run[i+1].Time - run[i].Time
	}
	cr := ChunkRow{
		Type:  chunkTagOfKind(kind),
		Seq0:  first.Seq,
		Time0: first.Time,
		Data:  RunData{Turn: turn, Step: step, Index: index, DT: dt},
	}
	switch kind {
	case "text-delta", "reasoning-delta":
		cr.Data.Texts = make([]string, len(run))
		for i, e := range run {
			_, _, _, _, _, _, t, _ := deltaFields(e)
			cr.Data.Texts[i] = t
		}
	case "tool-call-delta":
		cr.Data.ID = id
		cr.Data.Name = name
		cr.Data.Args = make([]string, len(run))
		for i, e := range run {
			_, _, _, _, _, _, _, a := deltaFields(e)
			cr.Data.Args[i] = a
		}
	}
	return cr
}

func emitBoundedRun(out []StorageRecord, kind string, run []*session.SessionEnvelope) []StorageRecord {
	offset := 0
	for len(run)-offset >= MIN_PACKED_ROW_MEMBERS {
		low := MIN_PACKED_ROW_MEMBERS
		high := len(run) - offset
		if high > MAX_PACKED_ROW_MEMBERS {
			high = MAX_PACKED_ROW_MEMBERS
		}
		cand := buildRow(kind, run[offset:offset+high])
		if packedDataBytes(cand) <= MAX_PACKED_DATA_BYTES {
			out = append(out, cand)
			offset += high
			continue
		}
		accepted := 0
		var acceptedRow ChunkRow
		hi := high
		for low <= hi {
			mid := (low + hi) / 2
			c := buildRow(kind, run[offset:offset+mid])
			if packedDataBytes(c) <= MAX_PACKED_DATA_BYTES {
				accepted = mid
				acceptedRow = c
				low = mid + 1
			} else {
				hi = mid - 1
			}
		}
		if accepted == 0 {
			out = append(out, run[offset])
			offset++
			continue
		}
		out = append(out, acceptedRow)
		offset += accepted
	}
	for _, e := range run[offset:] {
		out = append(out, e)
	}
	return out
}

// packChunkRuns converts logical events into scalar and packed physical records.
func packChunkRuns(events []*session.SessionEnvelope) []StorageRecord {
	out := make([]StorageRecord, 0, len(events))
	var kind string
	var run []*session.SessionEnvelope
	flush := func() {
		if kind == "" {
			for _, e := range run {
				out = append(out, e)
			}
		} else {
			out = emitBoundedRun(out, kind, run)
		}
		kind = ""
		run = nil
	}
	for _, env := range events {
		next := classifyDelta(env)
		if next == "" {
			flush()
			out = append(out, env)
			continue
		}
		if next == kind && len(run) > 0 && continuesDelta(next, run[len(run)-1], env) {
			run = append(run, env)
			continue
		}
		flush()
		kind = next
		run = []*session.SessionEnvelope{env}
	}
	flush()
	return out
}

// decodeRow decodes one physical row into its complete logical event span.
// It fails loudly on malformed rows so scanRows can classify committed
// corruption versus a repairable torn tail (upstream decodeRow semantics).
func decodeRow(row *EventRow) ([]*session.SessionEnvelope, error) {
	if row.HasIgnorable && row.Ignorable != PACKED_ROW_SENTINEL {
		env, err := decodeScalarRow(row)
		if err != nil {
			return nil, err
		}
		return []*session.SessionEnvelope{env}, nil
	}
	switch row.Type {
	case "text-chunks", "reasoning-chunks", "tool-call-chunks":
		if row.SourceEventSeqs != nil || row.SurfaceOp != nil {
			return nil, fmt.Errorf("malformed %s storage row: packed surface fields must be null", row.Type)
		}
		return decodeSerializedChunkRow(row.Type, row.Seq, row.Time, decodeData(row.Data))
	default:
		if row.HasIgnorable {
			// ignorable == 0 is the packed sentinel: a non-chunk tag under it
			// is corruption, never a scalar row.
			return nil, fmt.Errorf("malformed %s storage row: packed discriminator requires a chunk tag", row.Type)
		}
		env, err := decodeScalarRow(row)
		if err != nil {
			return nil, err
		}
		return []*session.SessionEnvelope{env}, nil
	}
}

func decodeScalarRow(row *EventRow) (*session.SessionEnvelope, error) {
	env := &session.SessionEnvelope{
		Seq:  row.Seq,
		Time: row.Time,
		Type: row.Type,
		Data: json.RawMessage(decodeData(row.Data)),
	}
	if row.SourceEventSeqs != nil {
		seqs, err := decodeSourceEventSeqs(row.SourceEventSeqs)
		if err != nil {
			return nil, err
		}
		env.SourceEventSeqs = seqs
	}
	if row.SurfaceOp != nil {
		var op session.SurfaceOp
		if err := json.Unmarshal(row.SurfaceOp, &op); err != nil {
			// A malformed surface_op marks the row unreadable; returning the
			// error lets scanRows classify it as committed corruption or torn
			// tail instead of silently dropping the surface fields.
			return nil, fmt.Errorf("malformed surface_op at seq %d: %w", row.Seq, err)
		}
		env.SurfaceOp = &op
	}
	if row.HasIgnorable && row.Ignorable == 1 {
		env.Ignorable = true
	}
	return env, nil
}

func decodeData(data []byte) []byte {
	if !isZstdFrame(data) {
		return data
	}
	if dec, err := zstdDecode(data); err == nil {
		return dec
	}
	return data
}

func isZstdFrame(data []byte) bool {
	return len(data) >= 4 && binary.LittleEndian.Uint32(data[:4]) == 0xFD2FB528
}

func zstdDecode(data []byte) ([]byte, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(data, nil)
}

// decodeSerializedChunkRow expands one packed row into logical events,
// validating the packed shape and byte bound. The data object must carry the
// exact key set of its variant (upstream validateRow): text/reasoning rows are
// exactly {turn,step,index,dt,texts}; tool-call rows exactly
// {turn,step,index,id[,name],dt,args}. Malformed shapes error so scanRows can
// classify the row — never a silent zero-value reconstruction.
func decodeSerializedChunkRow(tag string, seq0 int, time0 int64, serialized []byte) ([]*session.SessionEnvelope, error) {
	if len(serialized) > MAX_PACKED_DATA_BYTES {
		return nil, fmt.Errorf("malformed %s storage row: data exceeds %d UTF-8 bytes", tag, MAX_PACKED_DATA_BYTES)
	}
	var raw struct {
		Turn  *json.Number    `json:"turn"`
		Step  *json.Number    `json:"step"`
		Index *int            `json:"index"`
		ID    *string         `json:"id"`
		Name  json.RawMessage `json:"name"`
		DT    []int64         `json:"dt"`
		Texts []string        `json:"texts"`
		Args  []string        `json:"args"`
	}
	if err := json.Unmarshal(serialized, &raw); err != nil {
		return nil, fmt.Errorf("malformed %s storage row: %v", tag, err)
	}
	dataKeys := jsonKeySet(serialized)
	if dataKeys == nil {
		return nil, fmt.Errorf("malformed %s storage row: data must be an object", tag)
	}
	var members []string
	hasName := false
	switch tag {
	case "tool-call-chunks":
		withName := hasExactKeys(dataKeys, "turn", "step", "index", "id", "name", "dt", "args")
		if !withName && !hasExactKeys(dataKeys, "turn", "step", "index", "id", "dt", "args") {
			return nil, fmt.Errorf("malformed %s storage row: invalid tool-call data fields", tag)
		}
		hasName = withName
		members = raw.Args
	case "text-chunks", "reasoning-chunks":
		if !hasExactKeys(dataKeys, "turn", "step", "index", "dt", "texts") {
			return nil, fmt.Errorf("malformed %s storage row: invalid text data fields", tag)
		}
		members = raw.Texts
	default:
		return nil, fmt.Errorf("malformed %s storage row: unknown tag", tag)
	}
	if raw.Turn == nil || raw.Step == nil || raw.Index == nil {
		return nil, fmt.Errorf("malformed %s storage row: turn/step/index must be numbers", tag)
	}
	if hasName {
		var nameVal string
		if err := json.Unmarshal(raw.Name, &nameVal); err != nil {
			return nil, fmt.Errorf("malformed %s storage row: id and optional name must be strings", tag)
		}
	}
	if len(members) < MIN_PACKED_ROW_MEMBERS || len(members) > MAX_PACKED_ROW_MEMBERS {
		return nil, fmt.Errorf("malformed %s storage row: member count out of range", tag)
	}
	if len(raw.DT) != len(members)-1 {
		return nil, fmt.Errorf("malformed %s storage row: dt length must match the member count", tag)
	}
	turn, errTurn := raw.Turn.Int64()
	step, errStep := raw.Step.Int64()
	if errTurn != nil || errStep != nil {
		return nil, fmt.Errorf("malformed %s storage row: turn/step must be numbers", tag)
	}
	index := *raw.Index
	kind := map[string]string{
		"text-chunks": "text-delta", "reasoning-chunks": "reasoning-delta", "tool-call-chunks": "tool-call-delta",
	}[tag]
	var idVal string
	var namePtr *string
	if tag == "tool-call-chunks" {
		if raw.ID == nil {
			return nil, fmt.Errorf("malformed %s storage row: id and optional name must be strings", tag)
		}
		idVal = *raw.ID
		if hasName {
			var nameVal string
			if err := json.Unmarshal(raw.Name, &nameVal); err != nil {
				return nil, fmt.Errorf("malformed %s storage row: id and optional name must be strings", tag)
			}
			namePtr = &nameVal
		}
	}
	out := make([]*session.SessionEnvelope, 0, len(members))
	timeVal := time0
	for i := 0; i < len(members); i++ {
		if i > 0 {
			timeVal += raw.DT[i-1]
		}
		chunk := map[string]any{"type": kind, "index": index}
		switch tag {
		case "text-chunks", "reasoning-chunks":
			chunk["text"] = members[i]
		case "tool-call-chunks":
			chunk["id"] = idVal
			if namePtr != nil {
				chunk["name"] = *namePtr
			}
			chunk["argumentsDelta"] = members[i]
		}
		payload, _ := json.Marshal(chunk)
		env := &session.SessionEnvelope{
			Seq:  seq0 + i,
			Time: timeVal,
			Type: session.EventAssistantChunk,
		}
		env.Data = json.RawMessage(fmt.Sprintf(`{"turn":%d,"step":%d,"chunk":%s}`, turn, step, payload))
		out = append(out, env)
	}
	return out, nil
}

// scanRows returns the contiguous logical prefix and optional torn-tail marker.
func scanRows(rows []*EventRow, base int) ([]*session.SessionEnvelope, *int, error) {
	lastTurnEndRow := -1
	for i := len(rows) - 1; i >= 0; i-- {
		ev, err := decodeRow(rows[i])
		if err == nil {
			for _, e := range ev {
				if e.Type == session.EventTurnEnd {
					lastTurnEndRow = i
					break
				}
			}
			if lastTurnEndRow >= 0 {
				break
			}
		}
	}
	var preserved []*session.SessionEnvelope
	expected := base
	for rowIndex := 0; rowIndex < len(rows); rowIndex++ {
		physical := rows[rowIndex]
		logical, err := decodeRow(physical)
		if err != nil {
			if rowIndex <= lastTurnEndRow {
				return nil, nil, fmt.Errorf("corrupt session log: invalid committed physical row at seq %d", physical.Seq)
			}
			return preserved, ptr(physical.Seq), nil
		}
		contiguous := true
		for _, ev := range logical {
			if ev.Seq != expected {
				contiguous = false
				break
			}
			expected++
		}
		if !contiguous {
			if rowIndex <= lastTurnEndRow {
				return nil, nil, fmt.Errorf("corrupt session log: invalid committed physical row at seq %d", physical.Seq)
			}
			return preserved, ptr(physical.Seq), nil
		}
		preserved = append(preserved, logical...)
	}
	return preserved, nil, nil
}

func ptr[T int](v T) *T { return &v }

// ---------------------------------------------------------------------------
// sourceEventSeqs varint codec (zig-zag deltas, mirrors upstream)
// ---------------------------------------------------------------------------

func encodeSourceEventSeqs(values []int) []byte {
	var buf []byte
	var previous uint64
	for i, v := range values {
		if v < 0 {
			panic("sourceEventSeqs must be non-negative")
		}
		value := uint64(v)
		var encoded uint64
		switch {
		case i == 0:
			encoded = value
		case value >= previous:
			encoded = (value - previous) * 2
		default:
			encoded = (previous-value)*2 - 1
		}
		buf = appendVarint(buf, encoded)
		previous = value
	}
	return buf
}

func appendVarint(buf []byte, v uint64) []byte {
	for v >= 0x80 {
		buf = append(buf, byte(v&0x7f)|0x80)
		v >>= 7
	}
	return append(buf, byte(v))
}

// maxSafeInteger mirrors JS Number.MAX_SAFE_INTEGER; the zig-zag space is
// bounded at twice that (upstream MAX_ZIGZAG_INTEGER).
const (
	maxSafeInteger    = int64(9_007_199_254_740_991)
	maxZigzagInteger  = uint64(18_014_398_509_481_982) // 2 * maxSafeInteger
)

func decodeSourceEventSeqs(raw []byte) ([]int, error) {
	values := []int{}
	var prev uint64
	off := 0
	first := true
	for off < len(raw) {
		decoded, next, err := readVarint(raw, off, first)
		if err != nil {
			return nil, err
		}
		off = next
		var delta int64
		if first {
			delta = int64(decoded)
			first = false
		} else if decoded&1 == 0 {
			delta = int64(decoded / 2)
		} else {
			delta = -int64((decoded + 1) / 2)
		}
		value := int64(prev) + delta
		if value < 0 || value > maxSafeInteger {
			return nil, fmt.Errorf("malformed source_event_seqs storage value: decoded seq is out of range")
		}
		values = append(values, int(value))
		prev = uint64(value)
	}
	return values, nil
}

// readVarint decodes one little-endian base-128 varint. It rejects truncated
// byte streams, non-canonical (overlong) encodings — a trailing continuation
// whose final payload byte is zero — values beyond the first/zig-zag limits,
// and shifts past 56 bits, mirroring upstream compression.ts readVarint.
func readVarint(bytes []byte, offset int, first bool) (value uint64, next int, err error) {
	limit := maxZigzagInteger
	if first {
		limit = uint64(maxSafeInteger)
	}
	var shift uint
	for offset < len(bytes) {
		b := bytes[offset]
		offset++
		value |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			if shift > 0 && b&0x7f == 0 {
				return 0, 0, fmt.Errorf("malformed source_event_seqs storage value: non-canonical varint")
			}
			if value > limit {
				return 0, 0, fmt.Errorf("malformed source_event_seqs storage value: varint is out of range")
			}
			return value, offset, nil
		}
		shift += 7
		if shift > 56 {
			return 0, 0, fmt.Errorf("malformed source_event_seqs storage value: varint is out of range")
		}
	}
	return 0, 0, fmt.Errorf("malformed source_event_seqs storage value: truncated varint")
}

// ---------------------------------------------------------------------------
// .jsonl.zstd reader (concatenated independent Zstandard frames)
// ---------------------------------------------------------------------------

type zstdFrameRange struct{ Start, End int }

// scanZstdFrames walks concatenated frames (magic, descriptor, block headers,
// checksum trailer) without decompressing. Returns complete frames and the
// start of a torn final frame (-1 when none).
func scanZstdFrames(data []byte) (frames []zstdFrameRange, tornStart int, err error) {
	const magic = 0xFD2FB528
	off := 0
	tornStart = -1
	for off < len(data) {
		start := off
		if len(data)-off < 4 {
			return frames, start, nil
		}
		if binary.LittleEndian.Uint32(data[off:off+4]) != magic {
			return nil, 0, fmt.Errorf("corrupt Zstandard session log: invalid frame magic at byte %d", off)
		}
		off += 4
		if off == len(data) {
			return frames, start, nil
		}
		desc := data[off]
		off++
		if desc&0x18 != 0 {
			return nil, 0, fmt.Errorf("corrupt Zstandard session log: reserved frame-header bit")
		}
		singleSegment := desc&0x20 != 0
		checksum := desc&0x04 != 0
		contentSizeFlag := desc >> 6
		dictionaryFlag := desc & 0x03
		dictionaryBytes := map[byte]int{0: 0, 1: 1, 2: 2, 3: 4}[dictionaryFlag]
		contentSizeBytes := 0
		switch {
		case contentSizeFlag == 0 && singleSegment:
			contentSizeBytes = 1
		case contentSizeFlag != 0:
			contentSizeBytes = 1 << contentSizeFlag
		}
		remaining := contentSizeBytes + dictionaryBytes
		if !singleSegment {
			remaining++
		}
		if len(data)-off < remaining {
			return frames, start, nil
		}
		off += remaining
		for {
			if len(data)-off < 3 {
				return frames, start, nil
			}
			blockHeader := uint32(data[off]) | uint32(data[off+1])<<8 | uint32(data[off+2])<<16
			off += 3
			last := blockHeader&1 != 0
			blockType := (blockHeader >> 1) & 0x03
			blockSize := int(blockHeader >> 3)
			if blockType == 0x03 {
				return nil, 0, fmt.Errorf("corrupt Zstandard session log: reserved block type")
			}
			payloadBytes := blockSize
			if blockType == 0x01 {
				payloadBytes = 1
			}
			if len(data)-off < payloadBytes {
				return frames, start, nil
			}
			off += payloadBytes
			if last {
				break
			}
		}
		if checksum {
			if len(data)-off < 4 {
				return frames, start, nil
			}
			off += 4
		}
		frames = append(frames, zstdFrameRange{Start: start, End: off})
	}
	return frames, tornStart, nil
}

// JsonlLog is a decoded session artifact: the header line plus event records.
// CommittedBytes is the byte offset of the end of the last fully
// newline-terminated record — the only safe append/truncation boundary
// (upstream SessionLogScanner.committedBytes).
type JsonlLog struct {
	Header         *session.SessionHeader
	Events         []*session.SessionEnvelope
	CommittedBytes int64
}

// ReadJsonlZstd parses a `.jsonl.zstd` artifact: one checksummed zstd frame per
// durable batch, the first frame carrying the `type:"session"` header line. A
// torn final frame yields the complete newline-terminated records it produced.
func ReadJsonlZstd(path string) (*JsonlLog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	frames, tornStart, err := scanZstdFrames(raw)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize zstd decoder: %w", err)
	}
	defer dec.Close()
	var plaintext bytes.Buffer
	for _, f := range frames {
		out, err := dec.DecodeAll(raw[f.Start:f.End], nil)
		if err != nil {
			return nil, fmt.Errorf("corrupt Zstandard session log: frame at byte %d decode/checksum failed: %w", f.Start, err)
		}
		plaintext.Write(out)
	}
	if tornStart >= 0 && tornStart < len(raw) {
		// Streaming read recovers available plaintext from a torn final frame.
		r, _ := zstd.NewReader(bytes.NewReader(raw[tornStart:]))
		if torn, terr := io.ReadAll(r); terr == nil {
			plaintext.Write(torn)
		}
		r.Close()
	}
	return parseJsonlRecords(plaintext.Bytes())
}

// parseJsonlRecords parses plaintext JSONL records with the upstream
// SessionLogScanner semantics:
//
//   - only newline-terminated records count as committed; a final fragment
//     without '\n' is a torn tail (dropped, never interpreted);
//   - CommittedBytes tracks the end of the last complete record;
//   - a record that fails to decode is an issue: scanning continues, but the
//     moment a later committed line carries a turn/end the issue escalates to
//     a hard error; if no turn/end follows, the preserved prefix up to the
//     issue is returned (the repairable-prefix policy);
//   - a logical seq gap in the contiguous prefix behaves the same way —
//     deferred unless a turn/end proves the region committed;
//   - malformed packed rows and bad surfaceOp values fail decoding exactly
//     like unparsable lines (never silently dropped).
func parseJsonlRecords(text []byte) (*JsonlLog, error) {
	headerEnd := bytes.IndexByte(text, '\n')
	if headerEnd < 0 {
		return nil, fmt.Errorf("empty or header-less session log")
	}
	headerRecord := text[:headerEnd+1]
	header, err := parseHeaderLine(headerRecord)
	if err != nil {
		return nil, err
	}

	var events []*session.SessionEnvelope
	var committed int64 = int64(headerEnd + 1)
	var issue error
	lineStart := headerEnd + 1
	eventLine := 0
	fail := func(format string, args ...any) error {
		return fmt.Errorf("corrupt session log: "+format, args...)
	}
	for lineStart < len(text) {
		nl := bytes.IndexByte(text[lineStart:], '\n')
		if nl < 0 {
			// Unterminated final record: torn tail, not committed. Stop.
			break
		}
		end := lineStart + nl
		line := bytes.TrimRight(text[lineStart:end], "\r")
		lineStart = end + 1
		if len(line) == 0 {
			continue
		}
		eventLine++
		thisLine := eventLine

		decoded, decErr := decodeStorageRecordValue(line)
		if decErr != nil {
			if json.Valid(line) {
				// Well-formed JSON failing structural validation is real
				// corruption — a torn write cannot produce a complete JSON
				// value — so it must surface immediately, never defer.
				return nil, fail("malformed committed event at line %d: %v", thisLine, decErr)
			}
			if issue == nil {
				issue = fail("unparsable committed event at line %d: %v", thisLine, decErr)
			}
			continue
		}
		if issue != nil {
			for _, ev := range decoded {
				if ev.Type == session.EventTurnEnd {
					return nil, issue // a turn/end proves the broken region committed
				}
			}
			continue
		}

		gapAt := -1
		for i, ev := range decoded {
			if ev.Seq != len(events) {
				gapAt = i
				break
			}
			events = append(events, ev)
		}
		if gapAt >= 0 {
			expected := len(events)
			events = events[:expected]
			issue = fail("seq gap in committed region at line %d (expected %d, got %d)",
				thisLine, expected, decoded[gapAt].Seq)
			for _, ev := range decoded {
				if ev.Type == session.EventTurnEnd {
					return nil, issue
				}
			}
			continue
		}
		committed = int64(lineStart)
	}
	if issue != nil && len(events) > 0 {
		// Preserve the recoverable prefix for repair paths; surface the issue
		// through the truncation boundary so callers can see the log was cut.
		return &JsonlLog{Header: header, Events: events, CommittedBytes: committed}, nil
	}
	return &JsonlLog{Header: header, Events: events, CommittedBytes: committed}, nil
}

// sessionFormatVersion is the JSONL session format version this build reads.
const sessionFormatVersion = 0

// parseHeaderLine parses and validates the `type: "session"`-tagged header
// record (with or without its trailing newline). Mirroring the upstream
// isHeaderLine guard + refuseForeignFormatVersion + fromHeaderLine:
//
//   - version != 0 is refused as an unsupported format (upgrade hint), before
//     any structural validation — a future format must not surface as corrupt;
//   - createdAt / delegationDepth must be non-negative integers;
//   - origin, when present, must be "subagent";
//   - retired sandboxMode/approvalPolicy fields are refused.
func parseHeaderLine(record []byte) (*session.SessionHeader, error) {
	line := bytes.TrimSpace(bytes.TrimRight(record, "\n"))
	var probe struct {
		Version *int   `json:"version"`
		ID      string `json:"id"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, fmt.Errorf("corrupt session log: header line is not valid JSON")
	}
	// Refuse a foreign format version before any structural check.
	var generic map[string]json.RawMessage
	_ = json.Unmarshal(line, &generic)
	if _, ok := generic["version"]; ok && probe.Version == nil {
		return nil, fmt.Errorf("session format unsupported for session %q: header version is not a number", probe.ID)
	}
	if probe.Version != nil && *probe.Version != sessionFormatVersion {
		return nil, fmt.Errorf("session format unsupported for session %q: version %d, this build reads %d",
			probe.ID, *probe.Version, sessionFormatVersion)
	}

	var h struct {
		Type            string  `json:"type"`
		Version         int     `json:"version"`
		ID              string  `json:"id"`
		CreatedAt       *int64  `json:"createdAt"`
		Cwd             *string `json:"cwd"`
		ParentSession   *string `json:"parentSession"`
		SeedLength      *int64  `json:"seedLength"`
		Origin          *string `json:"origin"`
		DelegationDepth *int    `json:"delegationDepth"`
		AgentPreset     *string `json:"agentPreset"`
	}
	if err := json.Unmarshal(line, &h); err != nil {
		return nil, fmt.Errorf("corrupt session log: header line is not valid JSON")
	}
	if _, ok := generic["sandboxMode"]; ok {
		return nil, fmt.Errorf("session header uses retired policy baseline fields")
	}
	if _, ok := generic["approvalPolicy"]; ok {
		return nil, fmt.Errorf("session header uses retired policy baseline fields")
	}
	if h.Type != "session" || h.ID == "" || h.CreatedAt == nil {
		return nil, fmt.Errorf("corrupt session log: first record is not a session header")
	}
	if *h.CreatedAt < 0 {
		return nil, fmt.Errorf("corrupt session log: header createdAt must be non-negative")
	}
	depth := 0
	if h.DelegationDepth != nil {
		if *h.DelegationDepth < 0 {
			return nil, fmt.Errorf("corrupt session log: header delegationDepth must be non-negative")
		}
		depth = *h.DelegationDepth
	}
	if h.Origin != nil && *h.Origin != "subagent" {
		return nil, fmt.Errorf("corrupt session log: header origin must be subagent when present")
	}
	header := &session.SessionHeader{
		Version: h.Version, ID: h.ID, CreatedAt: *h.CreatedAt, DelegationDepth: depth,
	}
	if h.Cwd != nil {
		header.Cwd = *h.Cwd
	}
	if h.ParentSession != nil {
		header.ParentSession = *h.ParentSession
	}
	if h.SeedLength != nil {
		if *h.SeedLength < 0 {
			return nil, fmt.Errorf("corrupt session log: header seedLength must be non-negative")
		}
		header.SeedLength = int(*h.SeedLength)
	}
	if h.Origin != nil {
		header.Origin = *h.Origin
	}
	if h.AgentPreset != nil {
		header.AgentPreset = *h.AgentPreset
	}
	return header, nil
}

// decodeStorageRecordValue parses one JSONL record: a scalar event or a packed
// chunk row. Malformed records (unparsable JSON, malformed packed rows, bad
// surfaceOp) return an error — the scanner owns the deferred-vs-fatal policy,
// and nothing is ever silently dropped (contract: 畸形行绝不静默丢).
func decodeStorageRecordValue(record []byte) ([]*session.SessionEnvelope, error) {
	var probe struct {
		Type  string          `json:"type"`
		Seq0  int             `json:"seq0"`
		Time0 int64           `json:"time0"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(record, &probe); err != nil {
		return nil, fmt.Errorf("record is not valid JSON")
	}
	switch probe.Type {
	case "text-chunks", "reasoning-chunks", "tool-call-chunks":
		row := &EventRow{
			Seq: probe.Seq0, Time: probe.Time0, Type: probe.Type,
			Data: probe.Data, IsText: true, HasIgnorable: true, Ignorable: PACKED_ROW_SENTINEL,
		}
		events, err := decodeRow(row)
		if err != nil {
			return nil, err
		}
		return events, nil
	default:
		var ev struct {
			Seq       int             `json:"seq"`
			Time      int64           `json:"time"`
			Type      string          `json:"type"`
			Data      json.RawMessage `json:"data"`
			Source    []int           `json:"sourceEventSeqs,omitempty"`
			SurfOp    json.RawMessage `json:"surfaceOp,omitempty"`
			Ignorable bool            `json:"ignorable,omitempty"`
		}
		if err := json.Unmarshal(record, &ev); err != nil {
			return nil, fmt.Errorf("event record is not valid JSON")
		}
		env := &session.SessionEnvelope{Seq: ev.Seq, Time: ev.Time, Type: ev.Type, Data: ev.Data}
		env.SourceEventSeqs = ev.Source
		if len(ev.SurfOp) > 0 {
			op, err := parseSurfaceOpJSON(ev.SurfOp)
			if err != nil {
				return nil, fmt.Errorf("malformed surfaceOp on event seq %d: %w", ev.Seq, err)
			}
			env.SurfaceOp = op
		}
		env.Ignorable = ev.Ignorable
		return []*session.SessionEnvelope{env}, nil
	}
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return int64(v)
}

// newUUIDString returns a random v4 UUID string.
func newUUIDString() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:16]))
}

// parseSurfaceOpJSON parses a surfaceOp wire value: the bare string "append"
// or the positional-replacement object. The surface_op column is stored as
// JSON.stringify(surfaceOp), so "append" arrives as the quoted string
// "\"append\"".
func parseSurfaceOpJSON(raw []byte) (*session.SurfaceOp, error) {
	var op session.SurfaceOp
	if err := json.Unmarshal(raw, &op); err != nil {
		return nil, err
	}
	if op.Op != "append" && op.Op != "replace" {
		return nil, fmt.Errorf("invalid surface op %q", op.Op)
	}
	return &op, nil
}

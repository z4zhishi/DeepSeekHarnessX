package storage

// store_jsonl.go
//
// Pure-Go JSONL (`.jsonl.zstd`) session-persistence WRITE backend, mirroring
// the upstream `session-persistence-jsonl` physical layout (format.ts /
// index.ts / zstd.ts). The schema-17 SQLite store remains the default
// durable path; this store is the drop-in companion that reproduces the
// upstream append-only artifact byte-for-byte:
//
//   - one file per session at
//     `<root>/--<projectKey>--/<encodeSegment(id)>/session.jsonl.zstd`;
//   - Zstandard container = concatenated independently-decodable frames, each
//     checksummed (ZSTD_c_checksumFlag=1, magic 0xFD2FB528), matching what
//     `ReadJsonlZstd` / `scanZstdFrames` already consume;
//   - the header record `{type:"session",version,id,createdAt,...}` lives in
//     its own first frame; the first durable batch is a separate second frame;
//     every later `append` adds one more frame (`eventLines + '\n'`);
//   - delta-chunk runs pack through the shared `packChunkRuns` codec into
//     `text-chunks` / `reasoning-chunks` / `tool-call-chunks` storage rows;
//   - torn-tail safety: an append rollback truncates to the previous committed
//     size on a partial write, and a crash repair truncates to the structural
//     torn-frame offset before restoring recovered events + synthetic closers.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dsh-go/pkg/session"
)

// jsonlCompressionSuffix is the physical artifact suffix for the zstd encoding.
const jsonlCompressionSuffix = ".jsonl.zstd"

// projectKeySlugMax mirrors upstream format.ts `projectKey` truncation (251
// codepoints of the readable slug before the `--`/`--` wrappers).
const projectKeySlugMax = 251

// JsonlStore writes upstream-compatible JSONL session artifacts. One
// instance owns one root; sessions group under human-readable project
// directories, then per-session directories.
type JsonlStore struct {
	root string
}

// OpenJsonlStore returns a JSONL backend rooted at root. The root is created
// lazily on the first materialization (upstream creates it on demand).
func OpenJsonlStore(root string) *JsonlStore {
	return &JsonlStore{root: root}
}

// Close is a no-op: JSONL is sequential media with no persistent handle.
func (s *JsonlStore) Close() error { return nil }

// ---------------------------------------------------------------------------
// Path helpers (mirror format.ts: encodeSegment / projectKey / logPath)
// ---------------------------------------------------------------------------

// encodeSegmentPath encodes a session id as a single safe path segment,
// injectively over ASCII (the realistic id space) with the upstream `~XXXX`
// escape for unsafe code units and a correct UTF-16 surrogate split for
// non-BMP code points. `.` and `..` are special-cased to defeat traversal.
func encodeSegmentPath(raw string) string {
	if raw == "" {
		return "~002E"
	}
	if raw == "." {
		return "~002E"
	}
	if raw == ".." {
		return "~002E~002E"
	}
	var out strings.Builder
	for _, r := range raw {
		if r != '~' && safeSegmentRune(r) {
			out.WriteRune(r)
			continue
		}
		appendHexEscape(&out, r)
	}
	return out.String()
}

func safeSegmentRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '.' || r == '_' || r == '-':
		return true
	}
	return false
}

// appendHexEscape writes one code point as UTF-16 code units escaped to
// `~XXXX` (surrogate split for non-BMP code points), matching upstream
// `charCodeAt` semantics.
func appendHexEscape(out *strings.Builder, r rune) {
	if r <= 0xFFFF {
		fmt.Fprintf(out, "~%04X", r)
		return
	}
	r -= 0x10000
	fmt.Fprintf(out, "~%04X~%04X", 0xD800+(r>>10), 0xDC00+(r&0x3FF))
}

// projectKey computes the human-readable, filesystem-safe project directory
// key for a cwd (upstream format.ts:147). Separators collapse to a single
// `-`; unsafe code units escape as `~XXXX`; the slug is bounded and wrapped.
func projectKey(cwd string) string {
	var readable strings.Builder
	separatorRun := false
	for _, r := range cwd {
		switch r {
		case '/', '\\', ':':
			if !separatorRun {
				readable.WriteByte('-')
			}
			separatorRun = true
		default:
			if r != '~' && safeSegmentRune(r) {
				readable.WriteRune(r)
			} else {
				appendHexEscape(&readable, r)
			}
			separatorRun = false
		}
	}
	slug := strings.TrimLeft(readable.String(), "-")
	if slug == "" {
		slug = "root"
	}
	if len(slug) > projectKeySlugMax {
		slug = slug[:projectKeySlugMax]
	}
	return "--" + slug + "--"
}

// projectDir returns the project directory under the root (upstream projectDir).
func (s *JsonlStore) projectDir(cwd string) string {
	if cwd == "" {
		return filepath.Join(s.root, "_no-cwd")
	}
	return filepath.Join(s.root, projectKey(cwd))
}

// sessionDir returns the per-session directory (upstream sessionDir).
func (s *JsonlStore) sessionDir(cwd, id string) string {
	return filepath.Join(s.projectDir(cwd), encodeSegmentPath(id))
}

// pathFor returns the artifact path for a session (upstream logPath).
func (s *JsonlStore) pathFor(cwd, id string) string {
	return filepath.Join(s.sessionDir(cwd, id), "session"+jsonlCompressionSuffix)
}

// ---------------------------------------------------------------------------
// Record serialization (mirrors upstream toHeaderLine / eventLines)
// ---------------------------------------------------------------------------

// jsonlHeaderLine is the `type:"session"` header record with the exact
// upstream key order. Optional fields are omitted (never null); delegationDepth
// is always present.
type jsonlHeaderLine struct {
	Type            string `json:"type"`
	Version         int    `json:"version"`
	ID              string `json:"id"`
	CreatedAt       int64  `json:"createdAt"`
	Cwd             string `json:"cwd,omitempty"`
	ParentSession   string `json:"parentSession,omitempty"`
	SeedLength      int    `json:"seedLength,omitempty"`
	Origin          string `json:"origin,omitempty"`
	DelegationDepth int    `json:"delegationDepth"`
	AgentPreset     string `json:"agentPreset,omitempty"`
}

func headerLine(meta *session.SessionHeader) jsonlHeaderLine {
	return jsonlHeaderLine{
		Type:            "session",
		Version:         meta.Version,
		ID:              meta.ID,
		CreatedAt:       meta.CreatedAt,
		Cwd:             meta.Cwd,
		ParentSession:   meta.ParentSession,
		SeedLength:      meta.SeedLength,
		Origin:          meta.Origin,
		DelegationDepth: meta.DelegationDepth,
		AgentPreset:     meta.AgentPreset,
	}
}

// encodeEventLines serializes a batch as JSONL records with no trailing
// newline, packing delta-chunk runs into storage rows (upstream eventLines).
func encodeEventLines(events []*session.SessionEnvelope) []byte {
	records := packChunkRuns(events)
	var buf bytes.Buffer
	for i, rec := range records {
		if i > 0 {
			buf.WriteByte('\n')
		}
		data, err := json.Marshal(rec)
		if err != nil {
			// Serialization is always valid for envelope/ChunkRow shapes.
			panic("jsonl: failed to marshal storage record: " + err.Error())
		}
		buf.Write(data)
	}
	return buf.Bytes()
}

// zstdFrame compresses one independently-decodable, checksummed zstd frame.
func zstdFrame(plain []byte) []byte {
	frame, err := zstdEncode(plain)
	if err != nil {
		panic("jsonl: zstd frame failed: " + err.Error())
	}
	return frame
}

// encodeMaterialization builds the full first-write artifact: a header frame
// plus a separate first-batch frame (upstream encodeMaterialization).
func encodeMaterialization(meta *session.SessionHeader, events []*session.SessionEnvelope) []byte {
	headerJSON, err := json.Marshal(headerLine(meta))
	if err != nil {
		panic("jsonl: header marshal failed: " + err.Error())
	}
	header := append(headerJSON, '\n')
	body := append(encodeEventLines(events), '\n')
	out := make([]byte, 0, len(header)+len(body)+8)
	out = append(out, zstdFrame(header)...)
	out = append(out, zstdFrame(body)...)
	return out
}

// encodeAppendBatch encodes one durable append as a single checksummed frame.
func encodeAppendBatch(events []*session.SessionEnvelope) []byte {
	return zstdFrame(append(encodeEventLines(events), '\n'))
}

// ---------------------------------------------------------------------------
// Write path (mirrors upstream materialize / appendLines / commitRepair)
// ---------------------------------------------------------------------------

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// AppendEvents durably appends a contiguous batch, materializing the artifact
// first when not yet present (upstream appendBatch semantics).
func (s *JsonlStore) AppendEvents(meta *session.SessionHeader, events []*session.SessionEnvelope) error {
	path := s.pathFor(meta.Cwd, meta.ID)
	exists, err := fileExists(path)
	if err != nil {
		return err
	}
	if !exists {
		return s.materialize(meta, events)
	}
	return s.appendLines(path, events)
}

// PutSession materializes a header-only log (for gateway compatibility). If a
// log already exists the header is left untouched (append-only semantics).
func (s *JsonlStore) PutSession(meta *session.SessionHeader) error {
	path := s.pathFor(meta.Cwd, meta.ID)
	exists, err := fileExists(path)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return s.materialize(meta, nil)
}

func (s *JsonlStore) DeleteSession(sessionID string) error {
	// Best-effort: remove any matching jsonl file across workspace dirs.
	if s == nil || sessionID == "" {
		return nil
	}
	var firstErr error
	_ = filepath.Walk(s.root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") && !strings.HasSuffix(path, ".jsonl.zstd") {
			return nil
		}
		if !strings.Contains(filepath.Base(path), sessionID) {
			return nil
		}
		if rmErr := os.Remove(path); rmErr != nil && firstErr == nil {
			firstErr = rmErr
		}
		return nil
	})
	return firstErr
}

// materialize atomically writes the header + first batch: temp-write, fsync,
// then publish to the final path. Refuses to publish over an existing log.
func (s *JsonlStore) materialize(meta *session.SessionHeader, events []*session.SessionEnvelope) error {
	path := s.pathFor(meta.Cwd, meta.ID)
	dir := s.sessionDir(meta.Cwd, meta.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("jsonl: failed to create session dir: %w", err)
	}
	exists, err := fileExists(path)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("jsonl: refusing to materialize %q: a log already exists on disk", path)
	}
	content := encodeMaterialization(meta, events)
	tmp := path + "." + newTmpSuffix() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("jsonl: failed to create temp log: %w", err)
	}
	cleanup := func() {
		_ = os.Remove(tmp)
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("jsonl: temp write failed: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("jsonl: temp sync failed: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("jsonl: temp close failed: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("jsonl: failed to publish log: %w", err)
	}
	return nil
}

// appendLines appends one checksummed zstd frame and fsyncs. On a partial
// write or sync failure it restores the previous committed size so an
// unchanged cursor cannot double-append duplicate sequence numbers.
func (s *JsonlStore) appendLines(path string, events []*session.SessionEnvelope) error {
	content := encodeAppendBatch(events)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("jsonl: failed to open log for append: %w", err)
	}
	before, statErr := f.Stat()
	rollback := func() error {
		if statErr != nil {
			return statErr
		}
		_ = f.Close()
		rf, err := os.OpenFile(path, os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer rf.Close()
		if err := rf.Truncate(before.Size()); err != nil {
			return err
		}
		return rf.Sync()
	}
	if _, err := f.Write(content); err != nil {
		rerr := rollback()
		_ = f.Close()
		if rerr != nil {
			return errors.Join(fmt.Errorf("jsonl: append write failed: %w", err), rerr)
		}
		return fmt.Errorf("jsonl: append write failed: %w", err)
	}
	if err := f.Sync(); err != nil {
		rerr := rollback()
		_ = f.Close()
		if rerr != nil {
			return errors.Join(fmt.Errorf("jsonl: append sync failed: %w", err), rerr)
		}
		return fmt.Errorf("jsonl: append sync failed: %w", err)
	}
	return f.Close()
}

// CommitRepair truncates a torn tail (when tornOffset != nil) to its byte
// offset, then appends recovered events plus synthetic closers (upstream
// commitRepair). The caller computes the torn byte offset from a prior read.
func (s *JsonlStore) CommitRepair(meta *session.SessionHeader, tornOffset *int64, recovered, closers []*session.SessionEnvelope) error {
	path := s.pathFor(meta.Cwd, meta.ID)
	if tornOffset != nil {
		f, err := os.OpenFile(path, os.O_WRONLY, 0600)
		if err != nil {
			return fmt.Errorf("jsonl: repair open failed: %w", err)
		}
		if err := f.Truncate(*tornOffset); err != nil {
			_ = f.Close()
			return fmt.Errorf("jsonl: repair truncate failed: %w", err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return fmt.Errorf("jsonl: repair sync failed: %w", err)
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	combined := make([]*session.SessionEnvelope, 0, len(recovered)+len(closers))
	combined = append(combined, recovered...)
	combined = append(combined, closers...)
	if len(combined) == 0 {
		return nil
	}
	return s.appendLines(path, combined)
}

// ---------------------------------------------------------------------------
// Read / list surface
// ---------------------------------------------------------------------------

// ReadArtifact decodes the log at the session's computed path (convenience
// wrapper over ReadJsonlZstd). Returns (nil, nil) when no artifact exists.
func (s *JsonlStore) ReadArtifact(meta *session.SessionHeader) (*JsonlLog, error) {
	path := s.pathFor(meta.Cwd, meta.ID)
	exists, err := fileExists(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return ReadJsonlZstd(path)
}

// ListSessions returns the headers of every materialized session under the
// root, mirroring upstream listArtifacts (header frame only; the full log is
// not parsed). Duplicate ids across project directories are rejected.
func (s *JsonlStore) ListSessions() ([]*session.SessionHeader, error) {
	projects, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	seen := map[string]string{}
	var out []*session.SessionHeader
	for _, proj := range projects {
		if !proj.IsDir() {
			continue
		}
		sessionDirs, err := os.ReadDir(filepath.Join(s.root, proj.Name()))
		if err != nil {
			return nil, err
		}
		for _, sd := range sessionDirs {
			if !sd.IsDir() {
				continue
			}
			artifact := filepath.Join(s.root, proj.Name(), sd.Name(), "session"+jsonlCompressionSuffix)
			exists, err := fileExists(artifact)
			if err != nil {
				return nil, err
			}
			if !exists {
				continue
			}
			first, err := readFirstZstdLine(artifact)
			if err != nil || first == nil {
				continue
			}
			header, err := parseHeaderBytes(first)
			if err != nil {
				continue
			}
			if prior, dup := seen[header.ID]; dup {
				return nil, fmt.Errorf("jsonl: duplicate session id %q in project dirs %q and %q", header.ID, prior, proj.Name())
			}
			seen[header.ID] = proj.Name()
			out = append(out, header)
		}
	}
	return out, nil
}

// readFirstZstdLine reads and validates the independently compressed header
// frame, returning its line without the trailing newline.
func readFirstZstdLine(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	frames, _, err := scanZstdFrames(raw)
	if err != nil || len(frames) == 0 {
		return nil, err
	}
	frame := raw[frames[0].Start:frames[0].End]
	plain, err := zstdDecode(frame)
	if err != nil {
		return nil, err
	}
	plain = bytes.TrimRight(plain, "\n\r")
	return plain, nil
}

// parseHeaderBytes parses a header record back into a SessionHeader, applying
// the same validation rules as the reader's parseHeaderLine (version, numeric
// domains, retired fields). Returns an error for a malformed/foreign header.
func parseHeaderBytes(line []byte) (*session.SessionHeader, error) {
	return parseHeaderLine(line)
}

// newTmpSuffix returns a random hex token for temp-file names.
func newTmpSuffix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

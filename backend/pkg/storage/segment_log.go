package storage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"dsh-go/pkg/session"
)

// SegmentLog provides an append-only, durable JSONL stream logger on disk.
type SegmentLog struct {
	filePath string
	header   *session.SessionHeader
	file     *os.File
	writer   *bufio.Writer
	mu       sync.Mutex
}

// OpenSegmentLog opens or creates a session.jsonl file for append-only streaming.
func OpenSegmentLog(dir string, header *session.SessionHeader) (*SegmentLog, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create session directory: %w", err)
	}

	filePath := filepath.Join(dir, "session.jsonl")
	fileExists := false
	if info, err := os.Stat(filePath); err == nil && info.Size() > 0 {
		fileExists = true
	}

	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open segment log: %w", err)
	}

	sl := &SegmentLog{
		filePath: filePath,
		header:   header,
		file:     file,
		writer:   bufio.NewWriterSize(file, 64*1024), // 64KB write buffer
	}

	// If new file, write header as Line 1
	if !fileExists && header != nil {
		headerBytes, err := json.Marshal(header)
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("failed to marshal session header: %w", err)
		}
		if _, err := sl.writer.Write(append(headerBytes, '\n')); err != nil {
			file.Close()
			return nil, fmt.Errorf("failed to write session header: %w", err)
		}
		if err := sl.writer.Flush(); err != nil {
			file.Close()
			return nil, fmt.Errorf("failed to flush session header: %w", err)
		}
	}

	return sl, nil
}

// Append persists a SessionEnvelope to the log file.
func (sl *SegmentLog) Append(env *session.SessionEnvelope) error {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("failed to marshal envelope: %w", err)
	}

	if _, err := sl.writer.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to write to segment log: %w", err)
	}

	return sl.writer.Flush()
}

// AppendBatch writes a slice of envelopes in one buffered batch.
func (sl *SegmentLog) AppendBatch(envs []*session.SessionEnvelope) error {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	for _, env := range envs {
		data, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("failed to marshal envelope: %w", err)
		}
		if _, err := sl.writer.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("failed to write envelope: %w", err)
		}
	}

	return sl.writer.Flush()
}

// ReadAll reads the complete session log: Header (Line 1) and all subsequent SessionEnvelopes.
func (sl *SegmentLog) ReadAll() (*session.SessionHeader, []session.SessionEnvelope, error) {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	// Flush pending writes first
	if err := sl.writer.Flush(); err != nil {
		return nil, nil, err
	}

	f, err := os.Open(sl.filePath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024) // up to 10MB line buffer

	var header *session.SessionHeader
	var envelopes []session.SessionEnvelope

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		if lineNum == 1 {
			var h session.SessionHeader
			if err := json.Unmarshal(line, &h); err == nil && h.ID != "" {
				header = &h
				continue
			}
		}

		var env session.SessionEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue // skip malformed lines during crash recovery
		}
		envelopes = append(envelopes, env)
	}

	if err := scanner.Err(); err != nil {
		return header, envelopes, err
	}

	return header, envelopes, nil
}

// Close closes the underlying writer and file descriptor.
func (sl *SegmentLog) Close() error {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	if err := sl.writer.Flush(); err != nil {
		sl.file.Close()
		return err
	}
	return sl.file.Close()
}
